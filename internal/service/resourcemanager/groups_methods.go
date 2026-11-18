package resourcemanager

// Code generated from the cloudidentity v1 discovery document of
// google.golang.org/api/cloudidentity/v1 (revision 20261019); the served
// methods are groups.go's (#1812). Regenerate it when the API changes.

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
	"cloudidentity.allowlistedDomains.create":                      {false, "POST", "v1/allowlistedDomains"},
	"cloudidentity.allowlistedDomains.delete":                      {false, "DELETE", "v1/allowlistedDomains/{allowlistedDomainsId}"},
	"cloudidentity.allowlistedDomains.get":                         {false, "GET", "v1/allowlistedDomains/{allowlistedDomainsId}"},
	"cloudidentity.allowlistedDomains.list":                        {false, "GET", "v1/allowlistedDomains"},
	"cloudidentity.customers.userinvitations.cancel":               {false, "POST", "v1/customers/{customersId}/userinvitations/{userinvitationsId}:cancel"},
	"cloudidentity.customers.userinvitations.get":                  {false, "GET", "v1/customers/{customersId}/userinvitations/{userinvitationsId}"},
	"cloudidentity.customers.userinvitations.isInvitableUser":      {false, "GET", "v1/customers/{customersId}/userinvitations/{userinvitationsId}:isInvitableUser"},
	"cloudidentity.customers.userinvitations.list":                 {false, "GET", "v1/customers/{customersId}/userinvitations"},
	"cloudidentity.customers.userinvitations.send":                 {false, "POST", "v1/customers/{customersId}/userinvitations/{userinvitationsId}:send"},
	"cloudidentity.devices.cancelWipe":                             {false, "POST", "v1/devices/{devicesId}:cancelWipe"},
	"cloudidentity.devices.create":                                 {false, "POST", "v1/devices"},
	"cloudidentity.devices.delete":                                 {false, "DELETE", "v1/devices/{devicesId}"},
	"cloudidentity.devices.deviceUsers.approve":                    {false, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:approve"},
	"cloudidentity.devices.deviceUsers.block":                      {false, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:block"},
	"cloudidentity.devices.deviceUsers.cancelWipe":                 {false, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:cancelWipe"},
	"cloudidentity.devices.deviceUsers.clientStates.get":           {false, "GET", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}/clientStates/{clientStatesId}"},
	"cloudidentity.devices.deviceUsers.clientStates.list":          {false, "GET", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}/clientStates"},
	"cloudidentity.devices.deviceUsers.clientStates.patch":         {false, "PATCH", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}/clientStates/{clientStatesId}"},
	"cloudidentity.devices.deviceUsers.delete":                     {false, "DELETE", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}"},
	"cloudidentity.devices.deviceUsers.get":                        {false, "GET", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}"},
	"cloudidentity.devices.deviceUsers.list":                       {false, "GET", "v1/devices/{devicesId}/deviceUsers"},
	"cloudidentity.devices.deviceUsers.lookup":                     {false, "GET", "v1/devices/{devicesId}/deviceUsers:lookup"},
	"cloudidentity.devices.deviceUsers.wipe":                       {false, "POST", "v1/devices/{devicesId}/deviceUsers/{deviceUsersId}:wipe"},
	"cloudidentity.devices.get":                                    {false, "GET", "v1/devices/{devicesId}"},
	"cloudidentity.devices.list":                                   {false, "GET", "v1/devices"},
	"cloudidentity.devices.wipe":                                   {false, "POST", "v1/devices/{devicesId}:wipe"},
	"cloudidentity.groups.create":                                  {true, "POST", "v1/groups"},
	"cloudidentity.groups.delete":                                  {true, "DELETE", "v1/groups/{groupsId}"},
	"cloudidentity.groups.get":                                     {true, "GET", "v1/groups/{groupsId}"},
	"cloudidentity.groups.getSecuritySettings":                     {false, "GET", "v1/groups/{groupsId}/securitySettings"},
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
	"cloudidentity.groups.updateSecuritySettings":                  {false, "PATCH", "v1/groups/{groupsId}/securitySettings"},
	"cloudidentity.inboundOidcSsoProfiles.create":                  {false, "POST", "v1/inboundOidcSsoProfiles"},
	"cloudidentity.inboundOidcSsoProfiles.delete":                  {false, "DELETE", "v1/inboundOidcSsoProfiles/{inboundOidcSsoProfilesId}"},
	"cloudidentity.inboundOidcSsoProfiles.get":                     {false, "GET", "v1/inboundOidcSsoProfiles/{inboundOidcSsoProfilesId}"},
	"cloudidentity.inboundOidcSsoProfiles.list":                    {false, "GET", "v1/inboundOidcSsoProfiles"},
	"cloudidentity.inboundOidcSsoProfiles.patch":                   {false, "PATCH", "v1/inboundOidcSsoProfiles/{inboundOidcSsoProfilesId}"},
	"cloudidentity.inboundSamlSsoProfiles.create":                  {false, "POST", "v1/inboundSamlSsoProfiles"},
	"cloudidentity.inboundSamlSsoProfiles.delete":                  {false, "DELETE", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}"},
	"cloudidentity.inboundSamlSsoProfiles.get":                     {false, "GET", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.add":      {false, "POST", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials:add"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.delete":   {false, "DELETE", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials/{idpCredentialsId}"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.get":      {false, "GET", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials/{idpCredentialsId}"},
	"cloudidentity.inboundSamlSsoProfiles.idpCredentials.list":     {false, "GET", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}/idpCredentials"},
	"cloudidentity.inboundSamlSsoProfiles.list":                    {false, "GET", "v1/inboundSamlSsoProfiles"},
	"cloudidentity.inboundSamlSsoProfiles.patch":                   {false, "PATCH", "v1/inboundSamlSsoProfiles/{inboundSamlSsoProfilesId}"},
	"cloudidentity.inboundSsoAssignments.create":                   {false, "POST", "v1/inboundSsoAssignments"},
	"cloudidentity.inboundSsoAssignments.delete":                   {false, "DELETE", "v1/inboundSsoAssignments/{inboundSsoAssignmentsId}"},
	"cloudidentity.inboundSsoAssignments.get":                      {false, "GET", "v1/inboundSsoAssignments/{inboundSsoAssignmentsId}"},
	"cloudidentity.inboundSsoAssignments.list":                     {false, "GET", "v1/inboundSsoAssignments"},
	"cloudidentity.inboundSsoAssignments.patch":                    {false, "PATCH", "v1/inboundSsoAssignments/{inboundSsoAssignmentsId}"},
	"cloudidentity.policies.create":                                {false, "POST", "v1/policies"},
	"cloudidentity.policies.delete":                                {false, "DELETE", "v1/policies/{policiesId}"},
	"cloudidentity.policies.get":                                   {false, "GET", "v1/policies/{policiesId}"},
	"cloudidentity.policies.list":                                  {false, "GET", "v1/policies"},
	"cloudidentity.policies.patch":                                 {false, "PATCH", "v1/policies/{policiesId}"},
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
