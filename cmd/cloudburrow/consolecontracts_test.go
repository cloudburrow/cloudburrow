package main

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// TestProvidersStillSatisfyTheInterfacesTheyImplement.
//
// Go interfaces are satisfied structurally, so a provider whose method signature
// drifts from the interface does not fail to compile: it silently stops
// satisfying it. When Driller gained a resource path, tasksProvider kept its old
// two-string Detail, compiled cleanly, and stopped being a Driller — so Cloud
// Tasks queues quietly became unopenable and nothing said so.
//
// These assertions are the compile-time check the language does not give us. A
// provider listed here that loses an interface fails the build of this test
// rather than losing a screen in silence.
func TestProvidersStillSatisfyTheInterfacesTheyImplement(t *testing.T) {
	// Drillers: every provider whose rows open.
	var _ console.Driller = tasksProvider{}
	var _ console.Driller = secretsProvider{}
	var _ console.Driller = runProvider{}
	var _ console.Driller = storageProvider{}
	var _ console.Driller = pubsubProvider{}
	var _ console.Driller = pubsubSubscriptionsProvider{}
	var _ console.Driller = pubsubSnapshotsProvider{}
	var _ console.Driller = firestoreProvider{}
	var _ console.Driller = datastoreProvider{}
	var _ console.Driller = bigtableProvider{}
	var _ console.Driller = spannerProvider{}
	var _ console.Driller = cloudSQLProvider{}
	var _ console.Driller = bigqueryProvider{}
	var _ console.OptionalDriller = kubeProvider{}
	var _ console.Driller = aiProvider{}
	var _ console.Driller = projectsProvider{}
	var _ console.Driller = kmsProvider{}
	var _ console.Driller = schedulerProvider{}

	// Creators.
	var _ console.Creator = tasksProvider{}
	var _ console.Creator = secretsProvider{}
	var _ console.Creator = runProvider{}
	var _ console.Creator = kmsProvider{}
	var _ console.Creator = schedulerProvider{}
	var _ console.PageCreator = secretsProvider{}
	var _ console.PageCreator = runProvider{}

	// Deleters.
	var _ console.Deleter = tasksProvider{}
	var _ console.Deleter = secretsProvider{}
	var _ console.Deleter = runProvider{}
	var _ console.Deleter = pubsubSubscriptionsProvider{}
	var _ console.Deleter = pubsubSnapshotsProvider{}

	// Actions, in both addressing modes.
	var _ console.Actor = tasksProvider{}
	var _ console.PathActor = tasksProvider{}
	var _ console.PathActor = secretsProvider{}
	var _ console.PathActor = spannerProvider{}
	var _ console.PathActor = kmsProvider{}
	var _ console.PathActor = pubsubSubscriptionsProvider{}
	var _ console.PathActor = schedulerProvider{}
	var _ console.Actor = schedulerProvider{}
	var _ console.Deleter = schedulerProvider{}

	// Editing and revealing.
	var _ console.Editor = secretsProvider{}
	var _ console.Editor = runProvider{}
	var _ console.Revealer = secretsProvider{}

	// Paging: the content listings that can continue a read past their first
	// page.
	var _ console.Pager = cloudSQLProvider{}
	var _ console.Pager = firestoreProvider{}
	var _ console.Pager = datastoreProvider{}
	var _ console.Pager = bigtableProvider{}

	// Queries: a statement for the SQL databases, a form for the ones with no
	// query language.
	var _ console.Executor = cloudSQLProvider{}
	var _ console.Executor = spannerProvider{}
	var _ console.Executor = bigqueryProvider{}
	var _ console.Builder = firestoreProvider{}
	var _ console.Builder = datastoreProvider{}
	var _ console.Builder = bigtableProvider{}
}
