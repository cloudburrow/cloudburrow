package main

// A pipeline run's runtime graph (#2205), as Google's run page shows it
// ("Visualize and analyze pipeline results",
// https://docs.cloud.google.com/vertex-ai/docs/pipelines/visualize-pipeline,
// read 2026-11-24): the page opens on the runtime graph of the run's
// steps, with its summary (basic run information and the run's
// parameters) in the Pipeline run analysis pane; choosing a step shows its
// execution details, input parameters and output parameters, and an
// artifact its type, storage location (URI) and metrics, with Open in ML
// Metadata.
//
// UNVERIFIED (re-read 2026-11-24,
// https://docs.cloud.google.com/vertex-ai/docs/pipelines/visualize-pipeline,
// which shows neither): how nested DAGs, conditions and
// loops are drawn (here a DAG node expands in place, each loop iteration a
// DAG of its own), the Expand artifacts switch's label, and how a cached
// step's status is worded ("Cached" here, the task SKIPPED with a CACHED
// execution).

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// kfpGraphSpec is what the graph needs of a KFP pipeline spec: each DAG's
// tasks and what they run after.
type kfpGraphSpec struct {
	Root       kfpGraphComp            `json:"root"`
	Components map[string]kfpGraphComp `json:"components"`
}

type kfpGraphComp struct {
	DAG *struct {
		Tasks map[string]kfpGraphTask `json:"tasks"`
	} `json:"dag"`
}

type kfpGraphTask struct {
	ComponentRef struct {
		Name string `json:"name"`
	} `json:"componentRef"`
	DependentTasks []string `json:"dependentTasks"`
	Inputs         struct {
		Parameters map[string]struct {
			TaskOutputParameter *struct {
				ProducerTask string `json:"producerTask"`
			} `json:"taskOutputParameter"`
			TaskFinalStatus *struct {
				ProducerTask string `json:"producerTask"`
			} `json:"taskFinalStatus"`
		} `json:"parameters"`
		Artifacts map[string]struct {
			TaskOutputArtifact *struct {
				ProducerTask string `json:"producerTask"`
			} `json:"taskOutputArtifact"`
		} `json:"artifacts"`
	} `json:"inputs"`
	ParameterIterator json.RawMessage `json:"parameterIterator"`
	ArtifactIterator  json.RawMessage `json:"artifactIterator"`
}

func (t kfpGraphTask) upstream() []string {
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" {
			seen[n] = true
		}
	}
	for _, d := range t.DependentTasks {
		add(d)
	}
	for _, p := range t.Inputs.Parameters {
		if p.TaskOutputParameter != nil {
			add(p.TaskOutputParameter.ProducerTask)
		}
		if p.TaskFinalStatus != nil {
			add(p.TaskFinalStatus.ProducerTask)
		}
	}
	for _, a := range t.Inputs.Artifacts {
		if a.TaskOutputArtifact != nil {
			add(a.TaskOutputArtifact.ProducerTask)
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// task is the spec's task at a detail's unique name (DAG task names
// joined with dots, a loop's iteration its index), or false.
func (k kfpGraphSpec) task(unique string) (kfpGraphTask, bool, bool) {
	segs := strings.Split(unique, ".")
	comp := k.Root
	var t kfpGraphTask
	for i, seg := range segs {
		if _, err := strconv.Atoi(seg); err == nil && i > 0 {
			if i == len(segs)-1 {
				return kfpGraphTask{}, false, true // an iteration
			}
			continue
		}
		if comp.DAG == nil {
			return kfpGraphTask{}, false, false
		}
		var ok bool
		if t, ok = comp.DAG.Tasks[seg]; !ok {
			return kfpGraphTask{}, false, false
		}
		comp = k.Components[t.ComponentRef.Name]
	}
	return t, comp.DAG != nil, true
}

// pipelineNodeState is a node's status as the graph words it.
func pipelineNodeState(t *aiplatformpb.PipelineTaskDetail) string {
	if t.GetExecution().GetState() == aiplatformpb.Execution_CACHED {
		return "Cached"
	}
	switch t.GetState() {
	case aiplatformpb.PipelineTaskDetail_NOT_TRIGGERED:
		return "Not triggered"
	case aiplatformpb.PipelineTaskDetail_CANCEL_PENDING:
		return "Cancel pending"
	}
	return pipelineTaskStateText(t.GetState())
}

func valueText(v *structpb.Value) string {
	if s, ok := v.GetKind().(*structpb.Value_StringValue); ok {
		return s.StringValue
	}
	b, _ := protojson.Marshal(v)
	return string(b)
}

// metadataLink is the console route of an ML Metadata resource's page.
func metadataLink(name, region, project string) string {
	segs := strings.Split(name, "/")
	if len(segs) < 8 {
		return ""
	}
	tab := segs[6] // artifacts, executions or contexts
	return "/agent-platform/metadata/" + tab + "/" + segs[7] + "?project=" + project + "&region=" + region
}

// pipelineGraph is a run's graph for its page.
func pipelineGraph(j *aiplatformpb.PipelineJob, summary []console.PropertyGroup, project string) *console.PipelineGraph {
	var spec kfpGraphSpec
	if b, err := protojson.Marshal(j.GetPipelineSpec()); err == nil {
		_ = json.Unmarshal(b, &spec)
	}
	region := strings.Split(j.GetName(), "/")
	loc := ""
	if len(region) > 3 {
		loc = region[3]
	}
	g := &console.PipelineGraph{Summary: summary}
	details := j.GetJobDetail().GetTaskDetails()
	var rootID int64
	byID := map[int64]*aiplatformpb.PipelineTaskDetail{}
	for _, t := range details {
		byID[t.GetTaskId()] = t
		if t.GetParentTaskId() == 0 {
			rootID = t.GetTaskId()
		}
	}
	nodeID := func(id int64) string {
		if id == rootID || id == 0 {
			return ""
		}
		return "t" + strconv.FormatInt(id, 10)
	}
	// The tasks of each DAG detail by their task name, to resolve After.
	siblings := map[int64]map[string]int64{}
	for _, t := range details {
		if t.GetParentTaskId() == 0 {
			continue
		}
		segs := strings.Split(t.GetTaskUniqueName(), ".")
		if siblings[t.GetParentTaskId()] == nil {
			siblings[t.GetParentTaskId()] = map[string]int64{}
		}
		siblings[t.GetParentTaskId()][segs[len(segs)-1]] = t.GetTaskId()
	}
	artifactNodes := map[string]bool{}
	for _, t := range details {
		if t.GetParentTaskId() == 0 {
			continue
		}
		task, dag, known := spec.task(t.GetTaskUniqueName())
		kind := "task"
		switch {
		case !known:
		case task.ComponentRef.Name == "":
			kind = "iteration"
		case len(task.ParameterIterator) > 0 || len(task.ArtifactIterator) > 0:
			kind = "loop"
		case dag:
			kind = "dag"
		}
		n := console.PipelineNode{ID: nodeID(t.GetTaskId()), Parent: nodeID(t.GetParentTaskId()), Name: t.GetTaskName(), Kind: kind, State: pipelineNodeState(t)}
		for _, up := range task.upstream() {
			if id, ok := siblings[t.GetParentTaskId()][up]; ok {
				n.After = append(n.After, nodeID(id))
			}
		}
		for _, k := range sortedKeysOf(t.GetInputs()) {
			for _, a := range t.GetInputs()[k].GetArtifacts() {
				if a.GetName() != "" {
					n.Inputs = append(n.Inputs, "a:"+a.GetName())
				}
			}
		}
		info := []console.Property{
			{Label: "Name", Value: t.GetTaskName()},
			{Label: "Status", Value: n.State},
			{Label: "Created", Value: colabOptTime(t.GetCreateTime(), t.GetCreateTime() != nil)},
			{Label: "Started", Value: colabOptTime(t.GetStartTime(), t.GetStartTime() != nil)},
			{Label: "Ended", Value: colabOptTime(t.GetEndTime(), t.GetEndTime() != nil)},
		}
		if e := t.GetExecution(); e.GetName() != "" {
			info = append(info, console.Property{Label: "Execution", Value: vertexLast(e.GetName()) + " (" + e.GetSchemaTitle() + ")", Link: metadataLink(e.GetName(), loc, project)})
		}
		attempts := 0
		for _, s := range t.GetPipelineTaskStatus() {
			if s.GetState() == aiplatformpb.PipelineTaskDetail_RUNNING {
				attempts++
			}
		}
		if attempts > 1 {
			info = append(info, console.Property{Label: "Attempts", Value: strconv.Itoa(attempts)})
		}
		if msg := t.GetError().GetMessage(); msg != "" {
			info = append(info, console.Property{Label: "Error", Value: msg})
		}
		n.Info = []console.PropertyGroup{{Heading: "Node info", Properties: info}}
		var ins, outs []console.Property
		meta := t.GetExecution().GetMetadata().GetFields()
		keys := make([]string, 0, len(meta))
		for k := range meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if name, ok := strings.CutPrefix(k, "input:"); ok {
				ins = append(ins, console.Property{Label: name, Value: valueText(meta[k])})
			} else if name, ok := strings.CutPrefix(k, "output:"); ok {
				outs = append(outs, console.Property{Label: name, Value: valueText(meta[k])})
			}
		}
		if len(ins) > 0 {
			n.Info = append(n.Info, console.PropertyGroup{Heading: "Input parameters", Properties: ins})
		}
		if len(outs) > 0 {
			n.Info = append(n.Info, console.PropertyGroup{Heading: "Output parameters", Properties: outs})
		}
		g.Nodes = append(g.Nodes, n)
		// The task's output artifacts, drawn after it.
		for _, k := range sortedKeysOf(t.GetOutputs()) {
			for _, a := range t.GetOutputs()[k].GetArtifacts() {
				id := "a:" + a.GetName()
				if a.GetName() == "" {
					id = "a:" + a.GetUri()
				}
				if artifactNodes[id] {
					continue
				}
				artifactNodes[id] = true
				props := []console.Property{
					{Label: "Name", Value: k},
					{Label: "Type", Value: dashIfEmpty(a.GetSchemaTitle())},
					{Label: "URI", Value: dashIfEmpty(a.GetUri()), Copy: "Copy URI"},
				}
				if a.GetName() != "" {
					props = append(props, console.Property{Label: "Artifact", Value: vertexLast(a.GetName()), Link: metadataLink(a.GetName(), loc, project)})
				}
				an := console.PipelineNode{ID: id, Parent: n.Parent, Name: k, Kind: "artifact", State: dashIfEmpty(a.GetSchemaTitle()), After: []string{n.ID},
					Info: []console.PropertyGroup{{Heading: "Node info", Properties: props}}}
				var mp []console.Property
				for _, mk := range sortedKeysOf(a.GetMetadata().GetFields()) {
					mp = append(mp, console.Property{Label: mk, Value: valueText(a.GetMetadata().GetFields()[mk])})
				}
				if len(mp) > 0 {
					heading := "Metadata"
					if strings.HasSuffix(a.GetSchemaTitle(), "Metrics") {
						heading = "Metrics"
					}
					an.Info = append(an.Info, console.PropertyGroup{Heading: heading, Properties: mp})
				}
				g.Nodes = append(g.Nodes, an)
			}
		}
	}
	// An artifact a task takes from another DAG level is drawn only there.
	for i, n := range g.Nodes {
		var keep []string
		for _, in := range n.Inputs {
			if artifactNodes[in] {
				keep = append(keep, in)
			}
		}
		g.Nodes[i].Inputs = keep
	}
	return g
}
