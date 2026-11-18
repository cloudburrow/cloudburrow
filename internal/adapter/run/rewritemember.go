package run

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// RewriteIAMMember replaces the member from with to (dropping it when to
// is empty) in the IAM policies this adapter keeps: Cloud Run services',
// jobs' and worker pools' (ConfigMaps of kind run-iam), Artifact Registry
// repositories' and Secure Source Manager instances' and repositories'. A
// deleted service account's bindings go through it (#1751), which the
// resource manager registry's OnMemberRewrite hook passes here.
func (s *Server) RewriteIAMMember(ctx context.Context, from, to string) error {
	var errs []error
	if s != nil && s.kn != nil {
		cms, err := listObjects[kconfigMap](ctx, s.kn, resConfigMap, labelOwned+"=true,"+labelKind+"="+kindRunIAM)
		if err != nil {
			errs = append(errs, err)
		}
		for _, cm := range cms {
			var st iampolicy.Stored
			if json.Unmarshal([]byte(cm.Data[iamDataKey]), &st) != nil || !st.RewriteMember(from, to) {
				continue
			}
			raw, err := json.Marshal(st)
			if err != nil {
				return err
			}
			patch, _ := json.Marshal(map[string]any{"data": map[string]string{iamDataKey: string(raw)}})
			if err := s.kn.Kube.Patch(ctx, resConfigMap, cm.Metadata.Name, string(patch)); err != nil {
				errs = append(errs, err)
			}
		}
	}
	rewriteRepositoryPolicies(from, to)
	rewriteSSMPolicies(from, to)
	return errors.Join(errs...)
}

func rewriteRepositoryPolicies(from, to string) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	changed := false
	for name, p := range repositories.policies {
		if p.RewriteMember(from, to) {
			repositories.policies[name] = p
			changed = true
		}
	}
	if changed {
		saveRepositoryPoliciesLocked()
	}
}

func rewriteSSMPolicies(from, to string) {
	ssmState.mu.Lock()
	defer ssmState.mu.Unlock()
	changed := false
	for name, p := range ssmState.policies {
		if p.RewriteMember(from, to) {
			ssmState.policies[name] = p
			changed = true
		}
	}
	if changed {
		saveSSMLocked()
	}
}
