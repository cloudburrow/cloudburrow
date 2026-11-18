package bigqueryfront

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/tfconvert"
)

// Imported TensorFlow models (#1775): CREATE MODEL … OPTIONS
// (MODEL_TYPE='TENSORFLOW', MODEL_PATH='gs://…' [, KMS_KEY_NAME]), as
// https://docs.cloud.google.com/bigquery/docs/reference/standard-sql/bigqueryml-syntax-create-tensorflow
// documents it (read 2026-11-18):
//
//   - "TensorFlow models must be in SavedModel format" and "Models must
//     be stored in Cloud Storage": MODEL_PATH names the SavedModel's
//     folder (gs://bucket/path/to/saved_model/*, as the page's examples
//     write it, or the folder without the *), whose saved_model.pb (or
//     .pbtxt) and variables/ are read from the instance's own Cloud
//     Storage;
//   - "Models are limited to 450 MB in size", the folder's files together;
//   - "Models are frozen at the time of model creation": the converted
//     model is kept with the model, never read again;
//   - "Only core TensorFlow operations are supported; models that use
//     custom or tf.contrib operations aren't supported": a graph whose op
//     TensorFlow does not register is 400;
//   - the type table: the integer types read and write INT64, the float
//     types FLOAT64, tf.bool BOOL and tf.string STRING; complex, quantized,
//     resource and variant tensors are unsupported (400);
//   - the functions the page says do not take a TensorFlow model,
//     ML.CONFUSION, ML.EVALUATE, ML.FEATURE, ML.ROC_CURVE,
//     ML.TRAINING_INFO and ML.WEIGHTS, are 400.
//
// How it runs: the front cannot run TensorFlow (it is CGO-less and static
// in the cluster), so the CLI's TensorFlow converter (internal/tfconvert,
// --port-tfconvert through cloudburrow-host, the front's --tfconvert)
// runs Google's tf.lite.TFLiteConverter.from_saved_model on its
// serving_default signature in a local container (deploy/tfconvert,
// `docker run --network none`), with TensorFlow Lite's builtin operators
// alone. The flatbuffer is kept with the model and runs on
// internal/tflitemodel exactly as an imported TENSORFLOW_LITE model does
// (modeltflite.go): its inputs and outputs are serving_default's.
//
// What CloudBurrow cannot run is 501, never a different answer: a graph
// that needs TensorFlow operators TensorFlow Lite has no builtin for
// (Flex, SELECT_TF_OPS) or that the converter otherwise cannot lower,
// with the converter's message; an operator internal/tflitemodel does not
// run; and every model without the CLI's converter.
//
// UNVERIFIED (docs re-read 2026-11-18: bigqueryml-syntax-create-tensorflow), as Google does not say: BigQuery's own error texts (the
// page names none but the out-of-memory one); "Models trained using a
// version of GraphDef earlier than version 20 aren't supported" is not
// checked; tf.train.Example inputs and SparseTensor inputs are 501 (the
// converter refuses them); ML.EXPLAIN_PREDICT and other functions the
// page does not name are 501; each row runs as a batch of one.

// tfConverter converts a SavedModel's files to TensorFlow Lite: the CLI's
// converter, *tfconvert.Client.
type tfConverter interface {
	Convert(ctx context.Context, files []tfconvert.File) ([]byte, error)
}

// tensorflowUnsupportedFns are the functions the create-tensorflow page
// says do not support TensorFlow models.
var tensorflowUnsupportedFns = map[string]bool{"CONFUSION_MATRIX": true, "CONFUSION": true, "EVALUATE": true,
	"FEATURE_INFO": true, "FEATURE": true, "ROC_CURVE": true, "TRAINING_INFO": true, "WEIGHTS": true}

// readSavedModel reads the SavedModel folder MODEL_PATH names.
func (f front) readSavedModel(ctx context.Context, path string) (string, []tfconvert.File, int, string) {
	if f.storage == nil {
		return "", nil, 404, "Not found: URI " + path + ": CloudBurrow was given no Cloud Storage to read it from; is Cloud Storage enabled? Nothing was run."
	}
	rest := strings.TrimPrefix(path, "gs://")
	bucket, name, _ := strings.Cut(rest, "/")
	if strings.Count(name, "*") > 1 || strings.Contains(strings.TrimSuffix(name, "*"), "*") {
		return "", nil, 400, "Invalid model_path " + path + ": a TensorFlow model's model_path is its SavedModel folder, gs://bucket/path/to/saved_model/*"
	}
	prefix := strings.TrimSuffix(name, "*")
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if bucket == "" || prefix == "" || prefix == "/" {
		return "", nil, 400, "Invalid model_path " + path + ": it must be gs://bucket/path/to/saved_model/*"
	}
	type object struct {
		Name string      `json:"name"`
		Size json.Number `json:"size"`
	}
	var found []object
	var total int64
	token := ""
	for {
		q := url.Values{"prefix": {prefix}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var list struct {
			Items         []object `json:"items"`
			NextPageToken string   `json:"nextPageToken"`
		}
		st, err := f.storage.do(ctx, "/storage/v1/b/"+url.PathEscape(bucket)+"/o?"+q.Encode(), &list)
		if err != nil {
			return "", nil, 404, f.storage.unreachable(path, err).Error()
		}
		if st != http.StatusOK {
			return "", nil, 404, fmt.Sprintf("Not found: URI %s (Cloud Storage answered HTTP %d). Nothing was run.", path, st)
		}
		for _, it := range list.Items {
			if strings.HasSuffix(it.Name, "/") {
				continue // a folder placeholder
			}
			n, _ := it.Size.Int64()
			total += n
			found = append(found, it)
		}
		if token = list.NextPageToken; token == "" {
			break
		}
	}
	uri := "gs://" + bucket + "/" + prefix
	if len(found) == 0 {
		return "", nil, 404, "Not found: URI " + path + ": the folder holds no files. Nothing was run."
	}
	if total > tfconvert.MaxModelBytes {
		return "", nil, 400, fmt.Sprintf("The TensorFlow model %s is %d bytes; models are limited to 450 MB in size", uri, total)
	}
	root := false
	for _, it := range found {
		if rel := strings.TrimPrefix(it.Name, prefix); rel == "saved_model.pb" || rel == "saved_model.pbtxt" {
			root = true
		}
	}
	if !root {
		return "", nil, 400, "Invalid model_path " + path + ": TensorFlow models must be in SavedModel format, and " + uri +
			" holds no saved_model.pb or saved_model.pbtxt"
	}
	var files []tfconvert.File
	for _, it := range found {
		rc, err := f.storage.open(ctx, gsObject{bucket, it.Name})
		if err != nil {
			return "", nil, 404, strings.Replace(err.Error(), "Nothing was loaded.", "Nothing was run.", 1)
		}
		data, err := io.ReadAll(io.LimitReader(rc, tfconvert.MaxModelBytes+1))
		rc.Close()
		if err != nil {
			return "", nil, 500, "cloudburrow: could not read gs://" + bucket + "/" + it.Name + ": " + err.Error()
		}
		files = append(files, tfconvert.File{Name: strings.TrimPrefix(it.Name, prefix), Data: data})
	}
	return uri, files, 0, ""
}

// tensorflowRefusal answers a SavedModel the converter refused.
func tensorflowRefusal(uri string, err error) (int, string) {
	var rf *tfconvert.Refusal
	if !errors.As(err, &rf) {
		return 501, "Not implemented here: the TensorFlow model " + uri + " could not be converted: " + err.Error() + ". Nothing was run."
	}
	switch rf.Kind {
	case "format", "signature":
		return 400, "Failed to load the TensorFlow model " + uri + ": TensorFlow models must be in SavedModel format: " + rf.Message
	case "custom":
		return 400, "Failed to load the TensorFlow model " + uri + ": only core TensorFlow operations are supported; models that use custom or " +
			"tf.contrib operations aren't supported: " + rf.Message
	case "limit":
		return 400, "The TensorFlow model " + uri + " is too large: models are limited to 450 MB in size"
	case "flex":
		return 501, "Not implemented here: the TensorFlow model " + uri + " needs TensorFlow operators that TensorFlow Lite has no builtin for " +
			"(it would need Flex, SELECT_TF_OPS), and CloudBurrow runs an imported TensorFlow model as TensorFlow Lite; the converter said: " +
			rf.Message + ". Nothing was run."
	case "unavailable":
		return 501, "Not implemented here: the TensorFlow model " + uri + ": CloudBurrow's TensorFlow converter cannot run: " + rf.Message + ". Nothing was run."
	}
	return 501, "Not implemented here: the TensorFlow model " + uri + ": TensorFlow Lite's converter could not lower it: " + rf.Message + ". Nothing was run."
}

// importTensorFlow reads the SavedModel MODEL_PATH names, converts it to
// TensorFlow Lite and checks the result as importTFLite does.
func (f front) importTensorFlow(r *http.Request, d modelDDL) (*tfliteImport, []byte, int, string) {
	if f.tfconvert == nil {
		return nil, nil, 501, "Not implemented here: model_type TENSORFLOW: this BigQuery front was given no TensorFlow converter (--tfconvert), " +
			"which `cloudburrow up` serves with BigQuery enabled. Nothing was run."
	}
	uri, files, code, msg := f.readSavedModel(r.Context(), d.onnxPath)
	if code != 0 {
		return nil, nil, code, msg
	}
	data, err := f.tfconvert.Convert(r.Context(), files)
	if err != nil {
		code, msg := tensorflowRefusal(uri, err)
		return nil, nil, code, msg
	}
	imp, code, msg := tflitePorts(uri, data, "TensorFlow")
	if code != 0 {
		return nil, nil, code, msg
	}
	var size int64
	for _, f := range files {
		size += int64(len(f.Data))
	}
	imp.Size = size
	return imp, data, 0, ""
}

// tfliteSum is the hex SHA-256 of b.
func tfliteSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
