package compute

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Managed instance groups retry the VMs they fail to make (#2220), as
// Google's do: "By default, if the initial creation of a VM instance
// fails, the managed instance group continuously retries to create the
// VM, until the VM is successfully created"
// (compute/docs/instance-groups/add-remove-vms-in-mig). "If an action that
// is triggered by a MIG fails, the group might immediately retry that
// action. If the last immediate retry attempt fails, the MIG records the
// error ... After some backoff time, the group again retries the failed
// action" (compute/docs/instance-groups/getting-info-about-migs).
//
// A VM the group is making is a pending member of the group, kept on the
// manager under _pending by its URL, until its creation succeeds: it is
// listed by listManagedInstances (its instance URL kept, as Google's "URL
// can exist even if the instance has not yet been created", with no id
// and no instanceStatus while it does not exist), with currentAction
// CREATING (RECREATING when the group is re-making it), its last attempt's
// errors as lastAttempt.errors, and it keeps status.isStable false. Its
// failures after the immediate retry are kept for listErrors (the newest
// 500, of the last 8 days, as Google keeps).

const (
	migErrorsKept = 500
	migErrorsAge  = 8 * 24 * time.Hour
	// migRetryCap is the longest backoff between attempts.
	migRetryCap = time.Minute
)

// migRetryBase is the backoff after the immediate retry, doubling each
// failure up to migRetryCap. Google does not document its intervals.
func (s *Server) migRetryBase() time.Duration {
	if s.opts.MIGRetryBase > 0 {
		return s.opts.MIGRetryBase
	}
	return 5 * time.Second
}

// pendingOf is a manager's pending members by instance URL.
func pendingOf(m obj) map[string]any {
	p, _ := m["_pending"].(map[string]any)
	return p
}

func pendingEntry(m obj, vm string) obj {
	e, _ := pendingOf(m)[vm].(map[string]any)
	return e
}

// dropPendingLocked forgets a pending member: made, deleted, or its group
// gone.
func dropPendingLocked(m obj, vm string) {
	if p := pendingOf(m); p != nil {
		delete(p, vm)
		if len(p) == 0 {
			delete(m, "_pending")
		}
	}
}

// zoneOfRel is the zone of projects/P/zones/Z/instances/N.
func zoneOfRel(rel string) string {
	p := strings.Split(rel, "/")
	for i := 0; i+1 < len(p); i++ {
		if p[i] == "zones" {
			return p[i+1]
		}
	}
	return ""
}

// membersLocked are a manager's live VMs and the VMs it is still making.
func (s *Server) membersLocked(rel string, m obj) []string {
	out := s.liveManagedLocked(rel)
	for vm := range pendingOf(m) {
		if !hasStr(out, vm) {
			out = append(out, vm)
		}
	}
	sort.Strings(out)
	return out
}

// createManagedLocked makes a VM of manager mgr from template tpl, as
// action (CREATING or RECREATING): the VM is pending until it is made,
// and retried while its creation fails.
func (s *Server) createManagedLocked(project, mgr string, m obj, zone, name, tpl, action string) {
	vm := zoneRel(project, zone, "instances", name)
	e := pendingEntry(m, vm)
	if e == nil {
		if m["_pending"] == nil {
			m["_pending"] = map[string]any{}
		}
		e = obj{"zone": zone, "name": name, "action": action}
		pendingOf(m)[vm] = map[string]any(e)
	}
	e["template"], e["inFlight"] = tpl, true
	e["attempts"] = vpcFloat(e["attempts"]) + 1
	opRel, err := s.insertManagedVMFromLocked(project, zone, mgr, m, name, tpl)
	if err != nil {
		s.creationFailedLocked(mgr, m, vm, err.Error(), apiCode(err))
		return
	}
	if o := s.res[vm]; o != nil {
		mgrAction(o, action)
	}
	go func() {
		s.waitOp(context.Background(), opRel)
		s.mu.Lock()
		defer s.mu.Unlock()
		cur, ok := s.res[mgr]
		if !ok || pendingEntry(cur, vm) == nil {
			return // the group, or this member, deleted meanwhile
		}
		if _, made := s.res[vm]; made {
			dropPendingLocked(cur, vm)
			_ = s.saveLocked()
			return
		}
		code, msg := "RESOURCE_OPERATION_FAILED", "The instance was not created."
		if op := s.ops[opRel]; op != nil {
			oe, _ := op["error"].(map[string]any)
			if l := listOf(oe["errors"]); len(l) > 0 {
				first, _ := l[0].(map[string]any)
				code, msg = migErrorCode(str(first, "code")), str(first, "message")
			}
		}
		s.creationFailedLocked(mgr, cur, vm, msg, code)
		_ = s.saveLocked()
	}()
}

// creationFailedLocked records a failed attempt on a pending member and
// schedules its next one: at once after the first failure, then with a
// doubling backoff.
func (s *Server) creationFailedLocked(mgr string, m obj, vm, msg, code string) {
	e := pendingEntry(m, vm)
	if e == nil {
		return
	}
	now := s.opts.Now()
	attempts := int(vpcFloat(e["attempts"]))
	e["inFlight"] = false
	e["errors"] = []any{map[string]any{"code": code, "message": msg}}
	wait := time.Duration(0)
	if attempts >= 2 {
		wait = s.migRetryBase() << min(attempts-2, 16)
		if wait > migRetryCap || wait <= 0 {
			wait = migRetryCap
		}
		// The immediate retry failed too: the group records the error.
		errs := listOf(m["_errors"])
		errs = append(errs, map[string]any{
			"error":     map[string]any{"code": code, "message": "Instance '" + str(e, "name") + "' creation failed: " + msg},
			"timestamp": s.stamp(), "_at": float64(now.UnixNano()),
			"instanceActionDetails": map[string]any{"instance": vm, "action": str(e, "action"),
				"version": map[string]any{"instanceTemplate": str(e, "template")}}})
		if len(errs) > migErrorsKept {
			errs = errs[len(errs)-migErrorsKept:]
		}
		m["_errors"] = errs
	}
	e["next"] = float64(now.Add(wait).UnixNano())
}

// retryManagersLocked starts the attempts that are due, for every
// manager; it reports whether it started any.
func (s *Server) retryManagersLocked() bool {
	now := float64(s.opts.Now().UnixNano())
	did := false
	var mgrs []string
	for k, m := range s.res {
		if strings.Contains(k, "/instanceGroupManagers/") && len(pendingOf(m)) > 0 {
			mgrs = append(mgrs, k)
		}
	}
	sort.Strings(mgrs)
	for _, k := range mgrs {
		m := s.res[k]
		var due []string
		for vm, v := range pendingOf(m) {
			e, _ := v.(map[string]any)
			if e["inFlight"] == true || vpcFloat(e["next"]) > now {
				continue
			}
			if _, exists := s.res[vm]; exists {
				continue // a VM by its name (being deleted): wait
			}
			due = append(due, vm)
		}
		sort.Strings(due)
		for _, vm := range due {
			e := pendingEntry(m, vm)
			tpl := str(e, "template")
			if _, ok := s.res[tpl]; !ok {
				tpl = s.newVMTemplateLocked(k, m)
			}
			s.createManagedLocked(projectOfRel(k), k, m, str(e, "zone"), str(e, "name"), tpl, str(e, "action"))
			did = true
		}
	}
	return did
}

// migErrorCode is Google's operation error code for an error reason.
func migErrorCode(reason string) string {
	switch strings.ToLower(strings.ReplaceAll(reason, "_", "")) {
	case "notfound", "resourcenotfound":
		return "RESOURCE_NOT_FOUND"
	case "alreadyexists", "resourcealreadyexists":
		return "RESOURCE_ALREADY_EXISTS"
	case "invalid", "invalidfieldvalue":
		return "INVALID_FIELD_VALUE"
	case "resourceexhausted", "quotaexceeded":
		return "QUOTA_EXCEEDED"
	case "":
		return "RESOURCE_OPERATION_FAILED"
	}
	return reason
}

func apiCode(err error) string {
	if ae, ok := err.(*apiError); ok {
		return migErrorCode(ae.reason)
	}
	return "RESOURCE_OPERATION_FAILED"
}

// lastAttemptOf is a member's lastAttempt: its last attempt's errors.
func lastAttemptOf(e obj) obj {
	if errs := listOf(e["errors"]); len(errs) > 0 {
		return obj{"errors": obj{"errors": errs}}
	}
	return nil
}

// pendingManagedInstance is listManagedInstances' entry for a
// member not yet made.
func pendingManagedInstance(vm string, e obj) obj {
	mi := obj{"instance": vm, "name": str(e, "name"), "currentAction": str(e, "action"),
		"version": obj{"instanceTemplate": str(e, "template")}}
	if la := lastAttemptOf(e); la != nil {
		mi["lastAttempt"] = la
	}
	return mi
}

// listErrorsLocked is instanceGroupManagers.listErrors: the group's
// recorded errors, newest first, of the last 8 days.
func (s *Server) listErrorsLocked(c call, m obj) (obj, error) {
	if first(c.query, "filter") != "" || first(c.query, "orderBy") != "" {
		return nil, invalid("The `filter` and `orderBy` query parameters are not supported for this method.")
	}
	size := 500
	if v := first(c.query, "maxResults"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 500 {
			return nil, invalid("Invalid value for field 'maxResults': '%s'. Must be in the range [0, 500].", v)
		}
		if n > 0 {
			size = n
		}
	}
	from := 0
	if t := first(c.query, "pageToken"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return nil, invalid("Invalid value for field 'pageToken': '%s'.", t)
		}
		from = n
	}
	cutoff := s.opts.Now().Add(-migErrorsAge).UnixNano()
	var items []obj
	errs := listOf(m["_errors"])
	for i := len(errs) - 1; i >= 0; i-- {
		e, _ := errs[i].(map[string]any)
		if int64(vpcFloat(e["_at"])) < cutoff {
			continue
		}
		items = append(items, obj(e))
	}
	out := obj{}
	if from < len(items) {
		end := min(len(items), from+size)
		out["items"] = items[from:end]
		if end < len(items) {
			out["nextPageToken"] = strconv.Itoa(end)
		}
	}
	return out, nil
}
