package resourcemanager

// Code generated from the cloudidentity v1 discovery document of
// google.golang.org/api/cloudidentity/v1 (revision 20261019); the served
// methods are groups.go's (#1812), the security settings and inbound SSO
// (#1959), devices, user invitations, policies and allowlisted domains
// (#2103): every method. Regenerate it when the API changes.

// CloudIdentityMethod is one method of Cloud Identity v1: whether it is
// served, and its HTTP method and path.
type CloudIdentityMethod struct {
	Served     bool
	HTTPMethod string
	Path       string
}

// CloudIdentityMethods is every method of Cloud Identity v1; the ones not
// served answer 501.
var CloudIdentityMethods = map[string]CloudIdentityMethod{
	"cloudidentity.allowlistedDomains.create":                      {true, "POST", "v1/allowlistedDomains"},
	"cloudidentity.allowlistedDomains.delete":                      {true, "DELETE", "v1/allowlistedDomains/{allowlistedDomainsId}"},
	"cloudidentity.allowlistedDomains.get":                         {true, "GET", "v1/allowlistedDomains/{allowlistedDomainsId}"},
	"cloudidentity.allowlistedDomains.list":                        {true, "GET", "v1/allowlistedDomains"},
	"cloudidentity.customers.userinvitations.cancel":               {true, "POST", "v1/customers/{customersId}/userinvitations/{userinvitationsId}:cancel"},
	"cloudidentity.customers.userinvitations.get":                  {true, "GET", "v1/customers/{customersId}/userinvitations/{userinvitationsId}"},
	"cloudidentity.customers.userinvitations.isInvitableUser":      {true, "GET", "v1/customers/{customersId}/userinvitations/{userinvitationsId}:isInvitableUser"},
	"cloudidentity.customers.userinvitations.list":                 {true, "GET", "v1/customers/{customersId}/userinvitations"},
	"cloudidentity.customers.userinvitations.send":                 {true, "POST", "v1/customers/{customersId}/userinvitations/{userinvitationsId}:send"},
	"cloudidentity.devices.cancelWipe":                             {true, "POST", "v1/devices/{devicesId}:cancelWipe"},
	"cloudidentity.devices.create":                                 {true, "POST", "v1/devices"},
	"cloudidentity.devices.delete":                                 {true, "DELETE", "v1/devices/{devicesId}"},
	"cloudidentity.devices.deviceUsers.approve":                    {true, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:approve"},
	"cloudidentity.devices.deviceUsers.block":                      {true, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:block"},
	"cloudidentity.devices.deviceUsers.cancelWipe":                 {true, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:cancelWipe"},
	"cloudidentity.devices.deviceUsers.clientStates.get":           {true, "GET", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}/clientStates/{clientStatesId}"},
	"cloudidentity.devices.deviceUsers.clientStates.list":          {true, "GET", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}/clientStates"},
	"cloudidentity.devices.deviceUsers.clientStates.patch":         {true, "PATCH", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}/clientStates/{clientStatesId}"},
	"cloudidentity.devices.deviceUsers.delete":                     {true, "DELETE", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}"},
	"cloudidentity.devices.deviceUsers.get":                        {true, "GET", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}"},
	"cloudidentity.devices.deviceUsers.list":                       {true, "GET", "v1/devices/{devicesId}/deviceUsers"},
	"cloudidentity.devices.deviceUsers.lookup":                     {true, "GET", "v1/devices/{devicesId}/deviceUsers:lookup"},
	"cloudidentity.devices.deviceUsers.wipe":                       {true, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:wipe"},
	"cloudidentity.devices.get":                                    {true, "GET", "v1/devices/{devicesId}"},
	"cloudidentity.devices.list":                                   {true, "GET", "v1/devices"},
	"cloudidentity.devices.wipe":                                   {true, "POST", "v1/devices/{devicesId}:wipe"},
	"cloudidentity.groups.create":                                  {true, "POST", "v1/groups"},
	"cloudidentity.groups.delete":                                  {true, "DELETE", "v1/groups/{groupsId}"},
	"cloudidentity.groups.get":                                     {true, "GET", "v1/groups/{groupsId}"},
	"cloudidentity.groups.getSecuritySettings":                     {true, "GET", "v1/groups/{groupsId}/securitySettings"},
	"cloudidentity.groups.list":                                    {true, "GET", "v1/groups"},
	"cloudidentity.groups.lookup":                                  {true, "GET", "v1/groups:lookup"},
	"cloudidentity.groups.memberships.checkTransitiveMembership":   {true, "GET", "v1/groups/{groupsId}/memberships:checkTransitiveMembership"},
	"cloudidentity.groups.memberships.create":                      {true, "POST", "v1/groups/{groupsId}/memberships"},
	"cloudidentity.groups.memberships.delete":                      {true, "DELETE", "v1/groups/{groupsId}/memberships/{membershipsId}"},
	"cloudidentity.groups.memberships.get":                         {true, "GET", "v1/groups/{groupsId}/memberships/{membershipsId}"},
	"cloudidentity.groups.memberships.getMembershipGraph":          {true, "GET", "v1/groups/{groupsId}/memberships:getMembershipGraph"},
	"cloudidentity.groups.memberships.list":                        {true, "GET", "v1/groups/{groupsId}/memberships"},
	"cloudidentity.groups.memberships.lookup":                      {true, "GET", "v1/groups/{groupsId}/memberships:lookup"},
	"cloudidentity.groups.memberships.modifyMembershipRoles":       {true, "POST", "v1/groups/{groupsId}/memberships/{membershipsId}:modifyMembershipRoles"},
	"cloudidentity.groups.memberships.searchDirectGroups":          {true, "GET", "v1/groups/{groupsId}/memberships:searchDirectGroups"},
	"cloudidentity.groups.memberships.searchTransitiveGroups":      {true, "GET", "v1/groups/{groupsId}/memberships:searchTransitiveGroups"},
	"cloudidentity.groups.memberships.searchTransitiveMemberships": {true, "GET", "v1/groups/{groupsId}/memberships:searchTransitiveMemberships"},
	"cloudidentity.groups.patch":                                   {true, "PATCH", "v1/groups/{groupsId}"},
	"cloudidentity.groups.search":                                  {true, "GET", "v1/groups:search"},
	"cloudidentity.groups.updateSecuritySettings":                  {true, "PATCH", "v1/groups/{groupsId}/securitySettings"},
	"cloudidentity.inboundOidcSsoProfiles.create":                  {true, "POST", "v1/inboundOidcSsoProfiles"},
	"cloudidentity.inboundOidcSsoProfiles.delete":                  {true, "DELETE", "v1/inboundOidcSsoProfiles/{inboundOidcSsoProfilesId}"},
	"cloudidentity.inboundOidcSsoProfiles.get":                     {true, "GET", "v1/inboundOidcSsoProfiles/{inboundOidcSsoProfilesId}"},
	"cloudidentity.inboundOidcSsoProfiles.list":                    {true, "GET", "v1/inboundOidcSsoProfiles"},
	"cloudidentity.inboundOidcSsoProfiles.patch":                   {true, "PATCH", "v1/inboundOidcSsoProfiles/{inboundOidcSsoProfilesId}"},
	"cloudidentity.inboundSamlSsoProfiles.create":                  {true, "POST", "v1/inboundSamlSsoProfiles"},
	"cloudidentity.inboundSamlSsoProfiles.delete":                  {true, "DELETE", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}"},
	"cloudidentity.inboundSamlSsoProfiles.get":                     {true, "GET", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.add":      {true, "POST", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials:add"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.delete":   {true, "DELETE", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials/{idpCredentialsId}"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.get":      {true, "GET", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials/{idpCredentialsId}"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.list":     {true, "GET", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials"},
	"cloudidentity.inboundSamlSsoProfiles.list":                    {true, "GET", "v1/inboundSamlSsoProfiles"},
	"cloudidentity.inboundSamlSsoProfiles.patch":                   {true, "PATCH", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}"},
	"cloudidentity.inboundSsoAssignments.create":                   {true, "POST", "v1/inboundSsoAssignments"},
	"cloudidentity.inboundSsoAssignments.delete":                   {true, "DELETE", "v1/inboundSsoAssignments/{inboundSsoAssignmentsId}"},
	"cloudidentity.inboundSsoAssignments.get":                      {true, "GET", "v1/inboundSsoAssignments/{inboundSsoAssignmentsId}"},
	"cloudidentity.inboundSsoAssignments.list":                     {true, "GET", "v1/inboundSsoAssignments"},
	"cloudidentity.inboundSsoAssignments.patch":                    {true, "PATCH", "v1/inboundSsoAssignments/{inboundSsoAssignmentsId}"},
	"cloudidentity.policies.create":                                {true, "POST", "v1/policies"},
	"cloudidentity.policies.delete":                                {true, "DELETE", "v1/policies/{policiesId}"},
	"cloudidentity.policies.get":                                   {true, "GET", "v1/policies/{policiesId}"},
	"cloudidentity.policies.list":                                  {true, "GET", "v1/policies"},
	"cloudidentity.policies.patch":                                 {true, "PATCH", "v1/policies/{policiesId}"},
}

// CloudIdentityMethodStatuses is whether each method is served, for
// tools/coverage.
func CloudIdentityMethodStatuses() map[string]bool {
	out := make(map[string]bool, len(CloudIdentityMethods))
	for k, v := range CloudIdentityMethods {
		out[k] = v.Served
	}
	return out
}
