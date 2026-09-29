package bigqueryfront

import (
	"encoding/json"
	"net/http"
)

// clientLoad returns the client's configuration.load, as r's body has it,
// of a load job from Cloud Storage (uris), or nil (#998).
//
// The front reads such a load's files itself and sends the emulator a
// multipart upload of the job without its sourceUris (#944, gcsUpload),
// and the emulator keeps only some of a load's fields (measured: jobs.get
// gave destinationTable, schema, skipLeadingRows and sourceFormat, with no
// sourceUris, sourceColumnMatch or jobType). BigQuery keeps the job's
// configuration as it was sent (Job, JobConfigurationLoad.sourceUris), so
// the front puts the client's back in jobs.insert's answer, jobs.get and
// jobs.list (jobText.load).
func clientLoad(r *http.Request, uris []string) json.RawMessage {
	if len(uris) == 0 {
		return nil
	}
	body, err := readBody(r)
	if err != nil {
		return nil
	}
	var job struct {
		Configuration struct {
			Load json.RawMessage `json:"load"`
		} `json:"configuration"`
	}
	if json.Unmarshal(body, &job) != nil || len(job.Configuration.Load) == 0 || string(job.Configuration.Load) == "null" {
		return nil
	}
	return job.Configuration.Load
}
