package run

import (
	"context"
	"fmt"
	"regexp"
	"sort"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// A build's secrets (#1407), as Cloud Build reads them: availableSecrets'
// secretManager entries from this instance's Secret Manager, and its inline
// entries and the legacy secrets field decrypted with this instance's Cloud
// KMS, at build time. A step's secretEnv, and options.secretEnv for every
// arbitrary step, names which of them the step gets as environment
// variables. As on Google, Cloud Build does not mask a secret's value in
// the build log: a step that prints one shows it (the user's decision,
// 2026-10-16).

// SecretAccessor reads a Secret Manager secret version's payload, by its
// resource name. The CLI wires this instance's Secret Manager.
type SecretAccessor interface {
	AccessSecretVersion(ctx context.Context, versionName string) ([]byte, error)
}

// KMSDecrypter decrypts ciphertext with a Cloud KMS CryptoKey, by its
// resource name. The CLI wires this instance's Cloud KMS.
type KMSDecrypter interface {
	Decrypt(ctx context.Context, keyName string, ciphertext []byte) ([]byte, error)
}

// WithBuildSecrets sets where CreateBuild reads a build's secrets: nil
// refuses a build that names a secret of that kind FAILED_PRECONDITION,
// naming the service to enable.
func (b *BuildsServer) WithBuildSecrets(sm SecretAccessor, kms KMSDecrypter) *BuildsServer {
	b.secretManager, b.kms = sm, kms
	return b
}

var (
	secretVersionRE = regexp.MustCompile(`^projects/[^/]+/(locations/[^/]+/)?secrets/[^/]+/versions/[^/]+$`)
	cryptoKeyRE     = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/keyRings/[^/]+/cryptoKeys/[^/]+$`)
	envNameRE       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// buildSecret is one secret environment variable a build defines.
type buildSecret struct {
	env string
	// versionName is a Secret Manager version; else keyName and ciphertext
	// are a KMS-encrypted value.
	versionName, keyName string
	ciphertext           []byte
}

// planSecrets validates a build's secret definitions and that every
// secretEnv a step or options names is one of them, and returns them by
// env name.
func (c *CloudBuildServer) planSecrets(in *cbpb.Build, subs map[string]string) (map[string]buildSecret, error) {
	defined := map[string]buildSecret{}
	define := func(s buildSecret) error {
		if !envNameRE.MatchString(s.env) {
			return apierror.InvalidArgument("invalid build: secret env %q is not a valid environment variable name", s.env)
		}
		if _, dup := defined[s.env]; dup {
			return apierror.InvalidArgument("invalid build: secret env %q is defined more than once", s.env)
		}
		defined[s.env] = s
		return nil
	}
	kmsKey := func(field, name string) (string, error) {
		name = substitute(name, subs)
		if !cryptoKeyRE.MatchString(name) {
			return "", apierror.InvalidArgument("invalid build: %s %q is not a CryptoKey name, "+
				"projects/PROJECT/locations/LOCATION/keyRings/RING/cryptoKeys/KEY", field, name)
		}
		if c.builds.kms == nil {
			return "", apierror.FailedPrecondition("%s decrypts with this instance's Cloud KMS, which is not enabled: "+
				"start it with kms in --services", field)
		}
		return name, nil
	}
	for i, s := range in.GetAvailableSecrets().GetSecretManager() {
		name := substitute(s.GetVersionName(), subs)
		if !secretVersionRE.MatchString(name) {
			return nil, apierror.InvalidArgument("invalid build: availableSecrets.secretManager[%d].versionName %q is not a "+
				"secret version name, projects/PROJECT/secrets/SECRET/versions/VERSION", i, name)
		}
		if c.builds.secretManager == nil {
			return nil, apierror.FailedPrecondition("availableSecrets.secretManager reads this instance's Secret Manager, " +
				"which is not enabled: start it with secretmanager in --services")
		}
		if err := define(buildSecret{env: s.GetEnv(), versionName: name}); err != nil {
			return nil, err
		}
	}
	addKMS := func(field, key string, m map[string][]byte) error {
		name, err := kmsKey(field+".kmsKeyName", key)
		if err != nil {
			return err
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := define(buildSecret{env: k, keyName: name, ciphertext: m[k]}); err != nil {
				return err
			}
		}
		return nil
	}
	for i, s := range in.GetAvailableSecrets().GetInline() {
		if err := addKMS(fmt.Sprintf("availableSecrets.inline[%d]", i), s.GetKmsKeyName(), s.GetEnvMap()); err != nil {
			return nil, err
		}
	}
	for i, s := range in.GetSecrets() {
		if err := addKMS(fmt.Sprintf("secrets[%d]", i), s.GetKmsKeyName(), s.GetSecretEnv()); err != nil {
			return nil, err
		}
	}
	check := func(where string, names []string) error {
		for _, n := range names {
			if _, ok := defined[n]; !ok {
				return apierror.InvalidArgument("invalid build: %s names secret env %q, which availableSecrets "+
					"and secrets do not define", where, n)
			}
		}
		return nil
	}
	if err := check("options.secretEnv", in.GetOptions().GetSecretEnv()); err != nil {
		return nil, err
	}
	for i, st := range in.GetSteps() {
		if err := check(fmt.Sprintf("step %d secretEnv", i), st.GetSecretEnv()); err != nil {
			return nil, err
		}
	}
	return defined, nil
}

// resolveSecrets reads each secret the build defines, at build time.
func (r *buildRun) resolveSecrets(ctx context.Context) error {
	if len(r.secretDefs) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.secretDefs))
	for k := range r.secretDefs {
		names = append(names, k)
	}
	sort.Strings(names)
	values := map[string]string{}
	for _, n := range names {
		s := r.secretDefs[n]
		var v []byte
		var err error
		if s.versionName != "" {
			v, err = r.c.builds.secretManager.AccessSecretVersion(ctx, s.versionName)
			if err != nil {
				return fmt.Errorf("failed to access secret version %s: %v", s.versionName, firstLine(err.Error()))
			}
		} else {
			v, err = r.c.builds.kms.Decrypt(ctx, s.keyName, s.ciphertext)
			if err != nil {
				return fmt.Errorf("failed to decrypt secret env %s with %s: %v", n, s.keyName, firstLine(err.Error()))
			}
		}
		values[n] = string(v)
	}
	r.mu.Lock()
	r.secrets = values
	r.mu.Unlock()
	return nil
}
