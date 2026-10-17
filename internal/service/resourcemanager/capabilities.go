package resourcemanager

// Folder capabilities (#1519): cloudresourcemanager v3
// folders.capabilities.get and .patch, which Terraform's
// google_resource_manager_capability calls, to the discovery document and
// docs.cloud.google.com/resource-manager/docs/manage-applications. A
// capability is folders/{folder}/capabilities/{capability_name}, whose one
// allowed name is app-management, with a boolean value; a patch returns a
// long-running operation, done here when it returns, whose response is the
// Capability. A folder lists the capabilities set true on it as
// configuredCapabilities (folders.get on REST: the Go v3 Folder message of
// the official client has no such field).
//
// Enabling app-management makes the folder's management project, as Google
// documents (docs.cloud.google.com/resource-manager/docs/manage-applications,
// #1532): "The system creates the management project in the folder", "Each
// folder contains only one management project", and disabling
// "automatically and permanently deletes the management project". The
// folder names it as managementProject, and the project carries
// isManagementProject and configuredCapabilities, its folder's
// (cloudresourcemanager v3's discovery document), on REST: the Go v3
// messages have none of these fields. Its project ID is
// google-mp-{folder number}, shaped as the discovery document's one example
// (`projects/google-mp-123`): the ID Google assigns is not documented
// ("Make note of the project name and ID of the management project"). It is
// an ordinary project here, so a folder holding one is not empty, and the
// APIs Google enables on it and App Hub, which CloudBurrow does not serve,
// are not modelled. v3 projects.get names isManagementProject and
// configuredCapabilities; projects.list and search do not (UNVERIFIED
// there).
// UNVERIFIED: the project ID and display name, the statuses and messages of
// the refusals, and the operation's metadata (UpdateCapabilityConfigMetadata,
// empty).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// appManagement is the capability that gives a folder a management project.
const appManagement = "app-management"

// folderCapabilities are the capability names a folder accepts.
var folderCapabilities = []string{appManagement}

// ManagementProjectID is the project ID of folder's management project
// (UNVERIFIED: shaped as the discovery document's example).
func ManagementProjectID(folder string) string { return "google-mp-" + folder }

// Capability is cloudresourcemanager v3's Capability.
type Capability struct {
	Name  string `json:"name"`
	Value bool   `json:"value"`
}

func capabilityName(folder, capability string) string {
	return "folders/" + folder + "/capabilities/" + capability
}

func checkCapability(capability string) error {
	if !slices.Contains(folderCapabilities, capability) {
		return apierror.InvalidArgument("capability %q is not one of %s", capability, strings.Join(folderCapabilities, ", "))
	}
	return nil
}

// FolderCapability returns a folder's capability; one never set is false.
func (r *Registry) FolderCapability(folder, capability string) (Capability, error) {
	if err := checkCapability(capability); err != nil {
		return Capability{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.loadFolder("folders/" + folder)
	if err != nil {
		return Capability{}, err
	}
	name := capabilityName(f.ID(), capability)
	return Capability{Name: name, Value: slices.Contains(f.Capabilities, name)}, nil
}

// SetFolderCapability sets a folder's capability.
func (r *Registry) SetFolderCapability(folder, capability string, value bool) (Capability, error) {
	if err := checkCapability(capability); err != nil {
		return Capability{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := r.loadFolder("folders/" + folder)
	if err != nil {
		return Capability{}, err
	}
	if f.State != StateActive {
		return Capability{}, apierror.FailedPrecondition("folder %s is pending deletion; undelete it before updating it", f.Name)
	}
	name := capabilityName(f.ID(), capability)
	has := slices.Contains(f.Capabilities, name)
	switch {
	case value && !has:
		f.Capabilities = append(f.Capabilities, name)
		slices.Sort(f.Capabilities)
		if capability == appManagement {
			p, err := r.registerLocked(Project{ProjectID: ManagementProjectID(f.ID()), Parent: f.Name,
				ConfiguredCapabilities: []string{name}})
			if err != nil {
				return Capability{}, apierror.FailedPrecondition("cannot create the management project of %s: %v", f.Name, err)
			}
			f.ManagementProject = p.Name
		}
	case !value && has:
		f.Capabilities = slices.DeleteFunc(f.Capabilities, func(c string) bool { return c == name })
		if capability == appManagement && f.ManagementProject != "" {
			// Permanently: no DELETE_REQUESTED, no undelete.
			_ = r.st.Delete(key(strings.TrimPrefix(f.ManagementProject, "projects/")))
			r.dropIAM(f.ManagementProject)
			f.ManagementProject = ""
		}
	default:
		return Capability{Name: name, Value: value}, nil
	}
	f.UpdateTime = r.now().UTC().Format(time.RFC3339)
	return Capability{Name: name, Value: value}, r.putFolder(f)
}

// writeFolderJSON is a folder on REST, with configuredCapabilities, which
// the Go v3 Folder message lacks.
func writeFolderJSON(w http.ResponseWriter, f Folder) error {
	b, err := pj.Marshal(folderProto(f))
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	if len(f.Capabilities) > 0 || f.ManagementProject != "" {
		m := map[string]any{}
		if err := json.Unmarshal(b, &m); err != nil {
			return apierror.Internal(err, "encode response")
		}
		if len(f.Capabilities) > 0 {
			m["configuredCapabilities"] = f.Capabilities
		}
		if f.ManagementProject != "" {
			m["managementProject"] = f.ManagementProject
		}
		return rest.WriteJSON(w, http.StatusOK, m)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, err = w.Write(b)
	return err
}

// writeProjectJSON is a project on REST, with isManagementProject and
// configuredCapabilities when it is a management project, which the Go v3
// Project message lacks.
func (s *ProjectsServer) writeProjectJSON(w http.ResponseWriter, p *rmpb.Project) error {
	rec, err := s.reg.Get(p.GetProjectId())
	if err != nil || len(rec.ConfiguredCapabilities) == 0 {
		return writeProto(w, p)
	}
	b, err := pj.Marshal(p)
	if err != nil {
		return apierror.Internal(err, "encode response")
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return apierror.Internal(err, "encode response")
	}
	m["isManagementProject"] = true
	m["configuredCapabilities"] = rec.ConfiguredCapabilities
	return rest.WriteJSON(w, http.StatusOK, m)
}

func (s *FoldersServer) capabilityRoutes(r *rest.Router, ops *v1Ops) {
	r.Handle("GET /v3/folders/{folder}/capabilities/{capability}", func(w http.ResponseWriter, req *http.Request) error {
		c, err := s.reg.FolderCapability(req.PathValue("folder"), req.PathValue("capability"))
		if err != nil {
			return err
		}
		return rest.WriteJSON(w, http.StatusOK, c)
	})
	r.Handle("PATCH /v3/folders/{folder}/capabilities/{capability}", func(w http.ResponseWriter, req *http.Request) error {
		for _, p := range strings.Split(req.URL.Query().Get("updateMask"), ",") {
			if p = strings.TrimSpace(p); p != "" && p != "value" {
				return apierror.InvalidArgument("update_mask path %q cannot be updated; only value can", p)
			}
		}
		var in Capability
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 1<<20)).Decode(&in); err != nil {
			return apierror.InvalidArgument("decode body: %v", err)
		}
		folder, capability := req.PathValue("folder"), req.PathValue("capability")
		if in.Name != "" && in.Name != capabilityName(folder, capability) {
			return apierror.InvalidArgument("capability.name %q is not the path's %s", in.Name, capabilityName(folder, capability))
		}
		c, err := s.reg.SetFolderCapability(folder, capability, in.Value)
		if err != nil {
			return err
		}
		resp, _ := json.Marshal(map[string]any{"@type": "type.googleapis.com/google.cloud.resourcemanager.v3.Capability", "name": c.Name, "value": c.Value})
		meta, _ := json.Marshal(map[string]any{"@type": "type.googleapis.com/google.cloud.resourcemanager.v3.UpdateCapabilityConfigMetadata"})
		op := &crmv1.Operation{Name: fmt.Sprintf("operations/ucc.%s.%d", folder, time.Now().UnixNano()), Done: true, Response: resp, Metadata: meta}
		ops.put(op)
		return rest.WriteJSON(w, http.StatusOK, op)
	})
}
