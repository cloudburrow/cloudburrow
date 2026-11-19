package run

import (
	"strings"
	"testing"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestBuildAccount (#1854): a build runs as the default Cloud Build account
// without serviceAccount, and as the named one with it; a malformed name,
// a user-specified account logging to Google's default bucket, and an
// account IAM does not know are refused as Google refuses them; a unique ID
// resolves to its email.
func TestBuildAccount(t *testing.T) {
	UseServiceAccounts(func(name string) (string, bool, bool) {
		switch name {
		case "projects/p/serviceAccounts/builder@p.iam.gserviceaccount.com", "projects/p/serviceAccounts/1234":
			return "builder@p.iam.gserviceaccount.com", false, true
		case "projects/p/serviceAccounts/off@p.iam.gserviceaccount.com":
			return "off@p.iam.gserviceaccount.com", true, true
		}
		return "", false, false
	})
	t.Cleanup(func() { UseServiceAccounts(nil) })
	cloud := &cbpb.BuildOptions{Logging: cbpb.BuildOptions_CLOUD_LOGGING_ONLY}
	if got, err := buildAccount("p", &cbpb.Build{}); err != nil || got != DefaultBuildAccount("p") || !strings.HasSuffix(got, "@cloudbuild.gserviceaccount.com") {
		t.Errorf("no serviceAccount = %q, %v", got, err)
	}
	for _, c := range []struct {
		b    *cbpb.Build
		want string
		code codes.Code
	}{
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/builder@p.iam.gserviceaccount.com", Options: cloud}, "builder@p.iam.gserviceaccount.com", codes.OK},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/1234", LogsBucket: "gs://mine"}, "builder@p.iam.gserviceaccount.com", codes.OK},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/builder@p.iam.gserviceaccount.com",
			Options: &cbpb.BuildOptions{Logging: cbpb.BuildOptions_NONE}}, "builder@p.iam.gserviceaccount.com", codes.OK},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/9@cloudbuild.gserviceaccount.com", Options: cloud}, "9@cloudbuild.gserviceaccount.com", codes.OK},
		{&cbpb.Build{ServiceAccount: "builder@p.iam.gserviceaccount.com", Options: cloud}, "", codes.InvalidArgument},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/builder@p.iam.gserviceaccount.com"}, "", codes.InvalidArgument},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/builder@p.iam.gserviceaccount.com",
			Options: &cbpb.BuildOptions{Logging: cbpb.BuildOptions_GCS_ONLY}}, "", codes.InvalidArgument},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/nobody@p.iam.gserviceaccount.com", Options: cloud}, "", codes.InvalidArgument},
		{&cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/off@p.iam.gserviceaccount.com", Options: cloud}, "", codes.FailedPrecondition},
	} {
		got, err := buildAccount("p", c.b)
		if status.Code(err) != c.code || got != c.want {
			t.Errorf("buildAccount(%s) = %q, %v; want %q, %v", c.b.GetServiceAccount(), got, err, c.want, c.code)
		}
	}
	_, err := buildAccount("p", &cbpb.Build{ServiceAccount: "projects/p/serviceAccounts/builder@p.iam.gserviceaccount.com"})
	if err == nil || !strings.Contains(err.Error(), buildSALoggingError) {
		t.Errorf("the logging refusal = %v", err)
	}
}
