package run

import (
	"fmt"
	"regexp"
	"strings"

	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// A private instance (#1856), as Google's "Create a Private Service
// Connect instance" page documents it
// (https://docs.cloud.google.com/secure-source-manager/docs/create-private-service-connect-instance,
// read 2026-11-20): private_config.is_private, an optional ca_pool
// ("Required if you use your own CA certificate", in the form
// projects/CA_PROJECT/locations/CA_LOCATION/caPools/CA_POOL_NAME), and an
// optional custom_host_config of the instance's api, html, git_ssh and
// git_http domains, settable only at creation. Describe answers the
// instance's httpServiceAttachment and sshServiceAttachment, the targets a
// Private Service Connect endpoint's forwarding rule names.
//
// Locally there is no VPC between a client and the instance, so a private
// instance is served on this port as any other, and its host_config names
// this port: the custom domains are stored and answered, and resolve
// nowhere here, as they resolve only through the DNS records a user makes
// for the endpoints on Google.

// ssmCAPoolRE is a CA pool's resource name.
var ssmCAPoolRE = regexp.MustCompile(`^projects/[^/]+/locations/[^/]+/caPools/[^/]+$`)

// ssmHostRE is a custom domain name.
var ssmHostRE = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)

// ssmPrivateConfig checks an instance's private_config and fills in what
// the service sets on it: its service attachments. Output-only fields a
// caller sent are replaced.
func ssmPrivateConfig(in *ssmpb.Instance, name string) error {
	pc := in.GetPrivateConfig()
	if pc == nil {
		return nil
	}
	if !pc.GetIsPrivate() {
		if pc.GetCaPool() != "" || pc.GetCustomHostConfig() != nil {
			return apierror.InvalidArgument("private_config.ca_pool and custom_host_config need private_config.is_private")
		}
		pc.HttpServiceAttachment, pc.SshServiceAttachment = "", ""
		return nil
	}
	if ca := pc.GetCaPool(); ca != "" && !ssmCAPoolRE.MatchString(ca) {
		return apierror.InvalidArgument("invalid private_config.ca_pool %q: want projects/CA_PROJECT/locations/CA_LOCATION/caPools/CA_POOL_NAME", ca)
	}
	if ch := pc.GetCustomHostConfig(); ch != nil {
		for field, host := range map[string]string{"api": ch.GetApi(), "html": ch.GetHtml(), "git_ssh": ch.GetGitSsh(), "git_http": ch.GetGitHttp()} {
			if host != "" && !ssmHostRE.MatchString(host) {
				return apierror.InvalidArgument("invalid private_config.custom_host_config.%s %q: want a domain name", field, host)
			}
		}
	}
	project, location, err := resource.ParseLocation(name[:strings.Index(name, "/instances/")])
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	id := name[strings.LastIndex(name, "/")+1:]
	// The attachments live in the service's tenant project, whose name
	// Google does not document (UNVERIFIED, re-read 2026-11-20: the page
	// above shows them only as describe's output); here it is derived from
	// the project's number, so it is the same at every describe.
	base := fmt.Sprintf("projects/ssm-tp-%d/regions/%s/serviceAttachments/%s", resource.ProjectNumber(project), location, id)
	pc.HttpServiceAttachment, pc.SshServiceAttachment = base+"-http", base+"-ssh"
	return nil
}
