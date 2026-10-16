package main

// BigQuery ML models on a dataset's page (#1465): LINEAR_REG and binary
// LOGISTIC_REG models, which CREATE MODEL in the query editor's Read-write
// mode trains (the BQML rows of docs/compatibility.md). The dataset's page
// lists them (models.list); a model's page shows its type, features,
// label, training iterations, evaluation metrics and weights (models.get
// and ML.WEIGHTS); Edit model sets its description and friendly name
// (models.patch); Delete model drops it (models.delete), each through the
// official client against the forwarded REST port.
//
// A model's page is addressed [dataset, "models", model], as a routine's
// is [dataset, "routines", routine].

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const bigqueryModelsSegment = "models"

const (
	actDeleteModel = "deletemodel"
	actEditModel   = "editmodel"
)

func modelPath(datasetID, modelID string) []string {
	return []string{datasetID, bigqueryModelsSegment, modelID}
}

func isModelPath(path []string) bool {
	return len(path) == 3 && path[1] == bigqueryModelsSegment
}

func deleteModelAction() console.Action {
	return console.Action{ID: actDeleteModel, Label: "Delete model", Destructive: true, Leaves: true}
}

func editModelAction(md *bigquery.ModelMetadata) console.Action {
	return console.Action{ID: actEditModel, Label: "Edit model", Fields: []console.Field{
		{Name: "description", Label: "Description", Type: "text", Default: md.Description},
		{Name: "name", Label: "Friendly name", Type: "text", Default: md.Name},
	}}
}

// modelsSection lists a dataset's models, each opening its page.
func (p bigqueryProvider) modelsSection(ctx context.Context, ds *bigquery.Dataset) console.Section {
	out := console.Listing{Columns: []string{"Type", "Created"}, NameColumn: "Model", Noun: "models"}
	it := ds.Models(ctx)
	for {
		m, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			out.Unavailable = "listing models: " + bigqueryRefusal(err).Error()
			break
		}
		fields := map[string]string{"Type": "—", "Created": "—"}
		if md, err := m.Metadata(ctx); err == nil {
			fields["Type"] = orDash(md.Type)
			if !md.CreationTime.IsZero() {
				fields["Created"] = md.CreationTime.UTC().Format(time.RFC3339)
			}
		}
		out.Items = append(out.Items, console.Resource{
			Name: m.ModelID, Fields: fields,
			Opens:   modelPath(ds.DatasetID, m.ModelID),
			Actions: []console.Action{deleteModelAction()},
			ActsOn:  modelPath(ds.DatasetID, m.ModelID),
		})
		if len(out.Items) >= detailLimit {
			out.Note = truncatedNote(len(out.Items), "models")
			break
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	if out.Total == 0 && out.Unavailable == "" {
		out.Note = "This dataset has no models. CREATE MODEL ... OPTIONS(model_type='linear_reg' or 'logistic_reg') AS SELECT ... " +
			"in the query editor's Read-write mode trains one."
	}
	return console.Section{ID: "models", Label: "Models", Listing: out}
}

// modelDetail is a model's page.
func (p bigqueryProvider) modelDetail(ctx context.Context, datasetID, modelID string) (console.Detail, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	trail := []console.Crumb{{Label: datasetID, Path: []string{datasetID}}, {Label: modelID}}
	c, err := p.client(ctx)
	if err != nil {
		return console.Detail{Unavailable: err.Error(), Trail: trail}, nil
	}
	defer c.Close()
	md, err := c.Dataset(datasetID).Model(modelID).Metadata(ctx)
	if err != nil {
		return console.Detail{Unavailable: "cannot read the model: " + bigqueryRefusal(err).Error(), Trail: trail}, nil
	}
	feats := console.Listing{Columns: []string{"Type", "Weight"}, NameColumn: "Feature", Noun: "features"}
	cols, _ := md.RawFeatureColumns()
	weights := map[string]string{}
	q := c.Query(fmt.Sprintf("SELECT processed_input, weight, ARRAY_LENGTH(category_weights) AS n FROM ML.WEIGHTS(MODEL `%s.%s.%s`)",
		p.project, datasetID, modelID))
	if it, err := q.Read(ctx); err == nil {
		for {
			var row struct {
				Input  string               `bigquery:"processed_input"`
				Weight bigquery.NullFloat64 `bigquery:"weight"`
				N      int64                `bigquery:"n"`
			}
			if err := it.Next(&row); err != nil {
				break
			}
			if row.Weight.Valid {
				weights[row.Input] = strconv.FormatFloat(row.Weight.Float64, 'g', 8, 64)
			} else {
				weights[row.Input] = fmt.Sprintf("%d category weights", row.N)
			}
		}
	}
	for _, f := range cols {
		feats.Items = append(feats.Items, console.Resource{Name: f.Name, Fields: map[string]string{
			"Type": orDash(sqlType(f.Type)), "Weight": orDash(weights[f.Name])}})
	}
	if w, ok := weights["__INTERCEPT__"]; ok {
		feats.Items = append(feats.Items, console.Resource{Name: "__INTERCEPT__", Fields: map[string]string{"Type": "—", "Weight": w}})
	}
	feats.Total = len(feats.Items)

	iters := console.Listing{Columns: []string{"Loss", "Eval loss", "Learn rate", "Duration (ms)"}, NameColumn: "Iteration", Noun: "iterations"}
	var metrics []console.Property
	props := []console.Property{
		{Label: "Model ID", Value: p.project + "." + datasetID + "." + modelID},
		{Label: "Type", Value: orDash(md.Type)},
		{Label: "Location", Value: orDash(md.Location)},
	}
	if labels, _ := md.RawLabelColumns(); len(labels) > 0 {
		props = append(props, console.Property{Label: "Label column", Value: labels[0].Name + " " + sqlType(labels[0].Type)})
	}
	if runs := md.RawTrainingRuns(); len(runs) > 0 {
		run := runs[0]
		for _, r := range run.Results {
			iters.Items = append(iters.Items, console.Resource{Name: fmt.Sprint(r.Index), Fields: map[string]string{
				"Loss": strconv.FormatFloat(r.TrainingLoss, 'g', 8, 64), "Eval loss": evalLoss(r.EvalLoss),
				"Learn rate": strconv.FormatFloat(r.LearnRate, 'g', 6, 64), "Duration (ms)": fmt.Sprint(r.DurationMs)}})
		}
		if o := run.TrainingOptions; o != nil {
			props = append(props,
				console.Property{Label: "Optimization", Value: orDash(o.OptimizationStrategy)},
				console.Property{Label: "Data split", Value: orDash(o.DataSplitMethod)},
				console.Property{Label: "L1 / L2", Value: fmt.Sprintf("%g / %g", o.L1Regularization, o.L2Regularization)})
		}
		if em := run.EvaluationMetrics; em != nil {
			f := func(l string, v float64) {
				metrics = append(metrics, console.Property{Label: l, Value: strconv.FormatFloat(v, 'g', 8, 64)})
			}
			if rm := em.RegressionMetrics; rm != nil {
				f("Mean absolute error", rm.MeanAbsoluteError)
				f("Mean squared error", rm.MeanSquaredError)
				f("Median absolute error", rm.MedianAbsoluteError)
				f("R²", rm.RSquared)
			}
			if bm := em.BinaryClassificationMetrics; bm != nil && bm.AggregateClassificationMetrics != nil {
				a := bm.AggregateClassificationMetrics
				f("Precision", a.Precision)
				f("Recall", a.Recall)
				f("Accuracy", a.Accuracy)
				f("F1", a.F1Score)
				f("Log loss", a.LogLoss)
				f("ROC AUC", a.RocAuc)
			}
		}
	}
	iters.Total = len(iters.Items)
	if md.Description != "" {
		props = append(props, console.Property{Label: "Description", Value: md.Description})
	}
	if md.Name != "" {
		props = append(props, console.Property{Label: "Friendly name", Value: md.Name})
	}
	if len(md.Labels) > 0 {
		props = append(props, console.Property{Label: "Labels", Value: formatLabels(md.Labels)})
	}
	if !md.CreationTime.IsZero() {
		props = append(props, console.Property{Label: "Created", Value: md.CreationTime.UTC().Format(time.RFC3339)})
	}
	groups := []console.PropertyGroup{{Heading: "Model", Properties: props}}
	if len(metrics) > 0 {
		groups = append(groups, console.PropertyGroup{Heading: "Evaluation", Properties: metrics})
	}
	return console.Detail{
		Trail: trail,
		Summary: []console.Property{
			{Label: "Dataset", Value: datasetID},
			{Label: "Type", Value: orDash(md.Type)},
			{Label: "Features", Value: fmt.Sprint(len(cols))},
		},
		Sections: []console.Section{
			{ID: "features", Label: "Features", Listing: feats},
			{ID: "training", Label: "Training", Listing: iters},
			{ID: "details", Label: "Details", Kind: console.KindProperties, Groups: groups},
		},
	}, nil
}

func evalLoss(v float64) string {
	if v == 0 {
		return "—"
	}
	return strconv.FormatFloat(v, 'g', 8, 64)
}

// modelMetadata reads a model with models.get.
func (p bigqueryProvider) modelMetadata(ctx context.Context, datasetID, modelID string) (*bigquery.ModelMetadata, error) {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Dataset(datasetID).Model(modelID).Metadata(ctx)
}

// editModel sets a model's description and friendly name (models.patch).
func (p bigqueryProvider) editModel(ctx context.Context, datasetID, modelID string, values map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.Dataset(datasetID).Model(modelID).Update(ctx, bigquery.ModelMetadataToUpdate{
		Description: strings.TrimSpace(values["description"]), Name: strings.TrimSpace(values["name"])}, "")
	return bigqueryRefusal(err)
}

// deleteModel drops a model (models.delete).
func (p bigqueryProvider) deleteModel(ctx context.Context, datasetID, modelID string) error {
	ctx, cancel := context.WithTimeout(ctx, dbTimeout)
	defer cancel()
	c, err := p.client(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return bigqueryRefusal(c.Dataset(datasetID).Model(modelID).Delete(ctx))
}
