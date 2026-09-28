package console

import "context"

// A value an API answers with exactly once (#792): a Cloud Storage HMAC key's
// secret is in the create response and in no later one, so a console that
// did not show it then has lost it for good, and one that kept it would hold
// a credential the API itself no longer gives out.
//
// The value goes to the create response and nowhere else. The operations
// ledger names the resource created, the log entry says it was created, and
// neither is given the value; the page shows it in a dialog of its own and
// drops it when that closes.

// OneTime is a value shown once, with what identifies it.
type OneTime struct {
	// Title heads the dialog, such as "HMAC key created".
	Title string `json:"title"`
	// Properties identify what the value belongs to: an access ID, a service
	// account. None of them is secret.
	Properties []Property `json:"properties,omitempty"`
	// Label names the value, such as "Secret".
	Label string `json:"label"`
	// Value is the value itself.
	Value string `json:"value"`
	// Note says what happens once the dialog is closed.
	Note string `json:"note"`
}

// OneTimeCreator is a Creator whose create answers with a OneTime. When a
// provider implements it, the create route calls CreateOneTime instead of
// Create.
type OneTimeCreator interface {
	Creator
	// CreateOneTime makes the resource and returns its name and the value its
	// API returned once.
	CreateOneTime(ctx context.Context, project string, values map[string]string) (string, *OneTime, error)
}

// create calls the provider's one-time create when it has one.
func create(ctx context.Context, c Creator, project string, values map[string]string) (string, *OneTime, error) {
	if oc, ok := c.(OneTimeCreator); ok {
		return oc.CreateOneTime(ctx, project, values)
	}
	name, err := c.Create(ctx, project, values)
	return name, nil, err
}
