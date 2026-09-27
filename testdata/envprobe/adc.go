package main

// The /adc probe (#681): the rest of what the adapter injects, used the way
// an application on Cloud Run uses it.
//
//   - Application Default Credentials through golang.org/x/oauth2/google,
//     which, with no GOOGLE_APPLICATION_CREDENTIALS and no well-known file,
//     asks the metadata server at GCE_METADATA_HOST, as on real Cloud Run.
//     The token it returns is put on an official Storage call.
//   - Cloud KMS Encrypt, Cloud Scheduler ListJobs and Cloud Logging
//     WriteLogEntries at CLOUDBURROW_KMS_ENDPOINT,
//     CLOUDBURROW_SCHEDULER_ENDPOINT and CLOUDBURROW_LOGGING_ENDPOINT. No
//     client library reads those, so the address is given to the client,
//     as docs/credentials.md shows; the address itself is the injected one.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	kms "cloud.google.com/go/kms/apiv1"
	"cloud.google.com/go/kms/apiv1/kmspb"
	logging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	scheduler "cloud.google.com/go/scheduler/apiv1"
	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// adcPayload is what the probe encrypts and logs, for the host to find.
const adcPayload = "from-the-revision"

// bearerSeen counts the requests that carried a CloudBurrow token, so the
// probe reports that the token was sent rather than only minted. The token
// itself is never printed.
type bearerSeen struct{ n atomic.Int32 }

func (b *bearerSeen) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer cbl_") {
		b.n.Add(1)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// injected is an injected variable's value, or an error naming it.
func injected(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("%s was not injected", name)
	}
	return v, nil
}

// plaintext is the options for a CloudBurrow gRPC port: the address, and a
// plaintext channel, which is what those ports serve.
func plaintext(addr string) []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
}

// adcProbe runs the checks and reports what each returned. q names the
// bucket to list, the KMS key to encrypt with and the log to write to.
func adcProbe(ctx context.Context, q url.Values) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	project := os.Getenv("GOOGLE_CLOUD_PROJECT")
	var out []string

	// (a) ADC, from nothing but the environment.
	metadataHost, err := injected("GCE_METADATA_HOST")
	if err != nil {
		return "", err
	}
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", fmt.Errorf("google.FindDefaultCredentials: %w", err)
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", fmt.Errorf("a token from the metadata server: %w", err)
	}
	// A Google-issued token would mean the lookup reached Google.
	if !strings.HasPrefix(tok.AccessToken, "cbl_") {
		return "", fmt.Errorf("the ADC token was not minted by CloudBurrow")
	}
	seen := &bearerSeen{}
	hc := &http.Client{Transport: &oauth2.Transport{Source: creds.TokenSource, Base: seen}}
	sc, err := storage.NewClient(ctx, option.WithHTTPClient(hc))
	if err != nil {
		return "", fmt.Errorf("storage.NewClient with the ADC token: %w", err)
	}
	defer sc.Close()
	var objects []string
	it := sc.Bucket(q.Get("bucket")).Objects(ctx, nil)
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("list bucket %s with the ADC token: %w", q.Get("bucket"), err)
		}
		objects = append(objects, attrs.Name)
	}
	if seen.n.Load() == 0 {
		return "", fmt.Errorf("no Storage request carried the ADC token")
	}
	out = append(out, fmt.Sprintf("adc_project=%s token=local bearer_requests=%d objects=%s metadata=%s",
		creds.ProjectID, seen.n.Load(), strings.Join(objects, ","), metadataHost))

	// (b) KMS, Scheduler and Logging at the injected endpoints.
	kmsAddr, err := injected("CLOUDBURROW_KMS_ENDPOINT")
	if err != nil {
		return "", err
	}
	kc, err := kms.NewKeyManagementClient(ctx, plaintext(kmsAddr)...)
	if err != nil {
		return "", err
	}
	defer kc.Close()
	enc, err := kc.Encrypt(ctx, &kmspb.EncryptRequest{Name: q.Get("key"), Plaintext: []byte(adcPayload)})
	if err != nil {
		return "", fmt.Errorf("KMS Encrypt at %s: %w", kmsAddr, err)
	}
	out = append(out, "ciphertext="+base64.StdEncoding.EncodeToString(enc.GetCiphertext()), "kms="+kmsAddr)

	schedAddr, err := injected("CLOUDBURROW_SCHEDULER_ENDPOINT")
	if err != nil {
		return "", err
	}
	scc, err := scheduler.NewCloudSchedulerClient(ctx, plaintext(schedAddr)...)
	if err != nil {
		return "", err
	}
	defer scc.Close()
	var jobs []string
	jit := scc.ListJobs(ctx, &schedulerpb.ListJobsRequest{Parent: "projects/" + project + "/locations/us-central1"})
	for {
		j, err := jit.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return "", fmt.Errorf("Scheduler ListJobs at %s: %w", schedAddr, err)
		}
		jobs = append(jobs, j.GetName())
	}
	out = append(out, "jobs="+strings.Join(jobs, ","), "scheduler="+schedAddr)

	logAddr, err := injected("CLOUDBURROW_LOGGING_ENDPOINT")
	if err != nil {
		return "", err
	}
	lc, err := logging.NewClient(ctx, plaintext(logAddr)...)
	if err != nil {
		return "", err
	}
	defer lc.Close()
	logName := "projects/" + project + "/logs/" + q.Get("log")
	if _, err := lc.WriteLogEntries(ctx, &loggingpb.WriteLogEntriesRequest{
		LogName: logName, Resource: &mrpb.MonitoredResource{Type: "global"},
		Entries: []*loggingpb.LogEntry{{Payload: &loggingpb.LogEntry_TextPayload{TextPayload: adcPayload}}},
	}); err != nil {
		return "", fmt.Errorf("Logging WriteLogEntries at %s: %w", logAddr, err)
	}
	out = append(out, "logged="+logName, "logging="+logAddr)
	return strings.Join(out, " "), nil
}
