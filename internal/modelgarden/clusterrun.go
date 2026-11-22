package modelgarden

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/hfmodels"
)

// prepareCluster reads the model's card from the Hub, picks its engine from
// its task and library, and, for a gated model, checks that the deploy's
// token opens it, as Google's deploy does before it starts (its error:
// "Model ... is a gated Hugging Face model. The token is not valid or does
// not have permission").
func (m *Manager) prepareCluster(ctx context.Context, r Request) (hfmodels.Info, Engine, string, error) {
	info, eng, file, err := m.Choose(ctx, r.HFModelID)
	if err != nil {
		return hfmodels.Info{}, Engine{}, "", err
	}
	if info.Gated == "" {
		return info, eng, file, nil
	}
	card := hfmodels.Card{Revision: info.Revision, Files: info.Files}
	if r.HFToken == "" {
		return hfmodels.Info{}, Engine{}, "", apierror.InvalidArgument("Model %s is a gated Hugging Face model. Accept its licence "+
			"on huggingface.co/%s, then set model_config.hugging_face_access_token to a read token of the account that "+
			"accepted it", r.HFModelID, r.HFModelID)
	}
	if !r.AcceptEULA {
		return hfmodels.Info{}, Engine{}, "", apierror.FailedPrecondition("%s is gated on Hugging Face and needs its licence "+
			"accepted: set model_config.accept_eula to true", r.HFModelID)
	}
	if err := m.Models.CheckAccess(ctx, r.HFModelID, card.Revision, accessFile(card.Files), r.HFToken); err != nil {
		if errors.Is(err, hfmodels.ErrGated) {
			return hfmodels.Info{}, Engine{}, "", apierror.InvalidArgument("%s", strings.TrimPrefix(err.Error(), hfmodels.ErrGated.Error()+": "))
		}
		return hfmodels.Info{}, Engine{}, "", lookupError(r.HFModelID, errors.New(scrub(err.Error(), r.HFToken)))
	}
	return info, eng, file, nil
}

// Choose reads a Hub model's card and picks its runtime from its task and
// library, and the file that runtime loads for a GGUF or ONNX repository.
func (m *Manager) Choose(ctx context.Context, hfID string) (hfmodels.Info, Engine, string, error) {
	card, err := m.Models.Card(ctx, hfID)
	if err != nil {
		return hfmodels.Info{}, Engine{}, "", lookupError(hfID, err)
	}
	if card.Disabled {
		return hfmodels.Info{}, Engine{}, "", apierror.FailedPrecondition("%s is disabled on Hugging Face", hfID)
	}
	info := hfmodels.Info{ID: hfID, Revision: card.Revision, License: card.License, Gated: card.Gated,
		PipelineTag: card.Task, Files: card.Files, Bytes: card.Bytes}
	eng, err := EngineForModel(card.Task, card.Library, card.Tags)
	if err != nil {
		return hfmodels.Info{}, Engine{}, "", apierror.InvalidArgument("cannot deploy %s: %v", hfID, err)
	}
	file := ""
	switch eng.Name {
	case EngineLlamaCpp:
		if file = ggufFile(card.Files); file == "" {
			return hfmodels.Info{}, Engine{}, "", apierror.InvalidArgument("cannot deploy %s: the repository has no GGUF file", hfID)
		}
	case EngineONNX:
		if file = onnxFile(card.Files); file == "" {
			return hfmodels.Info{}, Engine{}, "", apierror.InvalidArgument("cannot deploy %s: the repository has no ONNX file", hfID)
		}
	}
	return info, eng, file, nil
}

// accessFile is the file a gated model's access is checked with: its
// config.json, or the first file that is not one Hugging Face serves to
// everyone.
func accessFile(files []hfmodels.File) string {
	for _, f := range files {
		if f.Path == "config.json" {
			return f.Path
		}
	}
	for _, f := range files {
		if f.Path != "README.md" && f.Path != ".gitattributes" {
			return f.Path
		}
	}
	return "config.json"
}

func (m *Manager) clusterTimeout() time.Duration {
	if m.ReadyTimeout > 0 {
		return m.ReadyTimeout
	}
	// The image pull (vLLM's CPU image is about 5.5 GB) and the weights'
	// download both happen inside the deploy.
	return 45 * time.Minute
}

// runCluster applies the workload, follows its pod through the image pull,
// the weights' download and the model's load, then opens its tunnel and
// checks the engine's health route through it.
func (m *Manager) runCluster(ctx context.Context, project, location string, p Prepared, model *Model, name string) error {
	eng, ok := EngineByName(model.Engine)
	if !ok {
		return apierror.FailedPrecondition("unknown serving engine %q", model.Engine)
	}
	m.setProgress(project, location, p.Endpoint, "Preparing the serving workload on the instance's cluster", 0)
	arch, err := m.Cluster.Arch(ctx)
	if err != nil {
		return apierror.FailedPrecondition("the instance's cluster cannot run the model: %v", err)
	}
	image := eng.ImageFor(arch)
	plan := Plan{HFID: model.HFModelID, Revision: model.Revision, Task: model.PipelineTag, File: model.File}
	if eng.Name == EngineVLLM || (eng.Name == EngineLlamaCpp && model.File != "") {
		if b, err := m.Models.ReadFile(ctx, model.HFModelID, model.Revision, "config.json", p.token, 1<<20); err == nil {
			plan.ContextLen = ContextLength(b)
		}
	}
	pod := eng.Pod(plan)
	m.mu.Lock()
	model.Image, model.Args, model.Env = image, append(append([]string{}, pod.Command...), pod.Args...), pod.Env
	m.save()
	m.mu.Unlock()
	w := Workload{Name: name, Image: image, Command: pod.Command, Args: pod.Args, Env: pod.Env, Files: pod.Files,
		ShmSize: eng.ShmSize, HealthRoute: eng.HealthRoute, Labels: map[string]string{
			"cloudburrow.dev/modelgarden-endpoint": p.Endpoint, "cloudburrow.dev/modelgarden-model": p.Model}}
	if err := m.Cluster.Apply(ctx, w, p.token); err != nil {
		return apierror.FailedPrecondition("%s", scrub(err.Error(), p.token))
	}
	return m.waitCluster(ctx, project, location, p.Endpoint, name, eng)
}

// waitCluster follows the workload until it is ready and its tunnel
// answers.
func (m *Manager) waitCluster(ctx context.Context, project, location, epID, name string, eng Engine) error {
	deadline := time.Now().Add(m.clusterTimeout())
	for {
		o, err := m.Cluster.Observe(ctx, name)
		if err == nil {
			switch o.Phase {
			case PhaseFailed:
				return apierror.FailedPrecondition("%s", o.Message)
			case PhaseMissing:
				return apierror.Aborted("the serving workload %s was deleted while it was being deployed", name)
			case PhasePulling:
				pct := 0
				msg := "Pulling the " + eng.Title + " image"
				if o.PullTotal > 0 {
					pct = int(o.Pulled * 100 / o.PullTotal)
					msg = fmt.Sprintf("Pulling the %s image (%s of %s)", eng.Title, mb(o.Pulled), mb(o.PullTotal))
				} else if o.Message != "" && o.Message != "Pulling the serving container image" {
					msg = o.Message
				}
				m.setProgress(project, location, epID, msg, pct)
			case PhaseReady:
				port, ferr := m.Cluster.Forward(ctx, name)
				if ferr == nil {
					m.mu.Lock()
					if ep := m.endpoints[key(project, location, epID)]; ep != nil && ep.Deployed != nil {
						ep.Deployed.HostPort = port
					}
					m.mu.Unlock()
					if m.healthy(ctx, port, eng) {
						return nil
					}
				}
				m.setProgress(project, location, epID, "Opening the tunnel to the serving pod", 0)
			default:
				m.setProgress(project, location, epID, o.Message, 0)
			}
		}
		if time.Now().After(deadline) {
			return &apierror.Error{Code: 4, Reason: "deadlineExceeded", Message: fmt.Sprintf(
				"the %s workload did not become ready within %s: %s", eng.Title, m.clusterTimeout(), m.Cluster.Logs(context.Background(), name, 12))}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func mb(n int64) string { return fmt.Sprintf("%.0f MB", float64(n)/1e6) }

// restoreCluster reopens a cluster deployment after an instance restart:
// its workload is still in the cluster (stop keeps it), so only the tunnel
// is new; a workload that is gone is applied again. A gated model's token
// is in its Secret, which a re-apply keeps; with the cluster deleted, the
// Secret went too, and the deployment fails asking for a redeploy.
func (m *Manager) restoreCluster(ctx context.Context, ep *Endpoint, model *Model, dm DeployedModel, p Prepared) {
	if m.Cluster == nil {
		m.mu.Lock()
		ep.Deployed.State, ep.Deployed.Message = StateFailed, "This deployment ran on the instance's cluster, which is not available"
		m.save()
		m.mu.Unlock()
		return
	}
	eng, _ := EngineByName(model.Engine)
	m.mu.Lock()
	ep.Deployed.State, ep.Deployed.Message = StateDeploying, "Reconnecting after an instance restart"
	m.mu.Unlock()
	o, err := m.Cluster.Observe(ctx, dm.Container)
	if err == nil && o.Phase == PhaseMissing {
		if dm.Gated {
			m.mu.Lock()
			ep.Deployed.State, ep.Deployed.Message = StateFailed, "The serving workload and its Hugging Face token Secret are gone "+
				"(the cluster was recreated). Deploy the model again with its token"
			m.save()
			m.mu.Unlock()
			return
		}
		_ = m.Run(ctx, ep.Project, ep.Location, p)
		return
	}
	err = m.waitCluster(ctx, ep.Project, ep.Location, ep.ID, dm.Container, eng)
	m.mu.Lock()
	if ep.Deployed != nil && ep.Deployed.ID == dm.ID {
		if err != nil {
			ep.Deployed.State, ep.Deployed.Message = StateFailed, err.Error()
		} else {
			ep.Deployed.State, ep.Deployed.Message, ep.Deployed.Progress = StateReady, "Ready", 100
		}
		m.save()
	}
	m.mu.Unlock()
}
