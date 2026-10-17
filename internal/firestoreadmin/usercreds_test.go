package firestoreadmin

import (
	"context"
	"testing"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUserCreds(t *testing.T) {
	ctx := context.Background()
	a := New("")
	if _, err := a.CreateDatabase(ctx, &adminpb.CreateDatabaseRequest{Parent: "projects/p", DatabaseId: "mongo",
		Database: &adminpb.Database{LocationId: "nam5", Type: adminpb.Database_FIRESTORE_NATIVE, DatabaseEdition: adminpb.Database_ENTERPRISE}}); err != nil {
		t.Fatal(err)
	}
	db := "projects/p/databases/mongo"
	uc, err := a.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: db, UserCredsId: "alice", UserCreds: &adminpb.UserCreds{}})
	if err != nil {
		t.Fatal(err)
	}
	if uc.State != adminpb.UserCreds_ENABLED || uc.SecurePassword == "" || uc.GetResourceIdentity().GetPrincipal() == "" {
		t.Errorf("created = %v", uc)
	}
	got, err := a.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: uc.Name})
	if err != nil || got.SecurePassword != "" {
		t.Errorf("get = %v, %v: want no password", got, err)
	}
	if _, err := a.CreateUserCreds(ctx, &adminpb.CreateUserCredsRequest{Parent: db, UserCredsId: "alice", UserCreds: &adminpb.UserCreds{}}); status.Code(err) != codes.AlreadyExists {
		t.Errorf("second create = %v, want ALREADY_EXISTS", err)
	}
	if d, err := a.DisableUserCreds(ctx, &adminpb.DisableUserCredsRequest{Name: uc.Name}); err != nil || d.State != adminpb.UserCreds_DISABLED {
		t.Errorf("disable = %v, %v", d, err)
	}
	r, err := a.ResetUserPassword(ctx, &adminpb.ResetUserPasswordRequest{Name: uc.Name})
	if err != nil || r.SecurePassword == "" || r.SecurePassword == uc.SecurePassword {
		t.Errorf("reset = %v, %v: want a new password", r, err)
	}
	if l, err := a.ListUserCreds(ctx, &adminpb.ListUserCredsRequest{Parent: db}); err != nil || len(l.UserCreds) != 1 {
		t.Errorf("list = %v, %v", l, err)
	}
	if _, err := a.DeleteUserCreds(ctx, &adminpb.DeleteUserCredsRequest{Name: uc.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetUserCreds(ctx, &adminpb.GetUserCredsRequest{Name: uc.Name}); status.Code(err) != codes.NotFound {
		t.Errorf("get after delete = %v, want NOT_FOUND", err)
	}
	if _, err := a.ListUserCreds(ctx, &adminpb.ListUserCredsRequest{Parent: "projects/p/databases/(default)"}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("list on a Standard database = %v, want FAILED_PRECONDITION", err)
	}
}
