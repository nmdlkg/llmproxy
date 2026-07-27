package handlers

import (
	"reflect"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/net/context"
)

func TestWithPreferredAuthIDsNormalizesAndCopies(t *testing.T) {
	input := []string{" auth-1 ", "", "auth-2"}
	ctx := WithPreferredAuthIDs(nil, input)
	input[0] = "changed"

	got := preferredAuthIDsFromContext(ctx)
	want := []string{"auth-1", "auth-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preferredAuthIDsFromContext() = %#v, want %#v", got, want)
	}
}

func TestWithPreferredAuthIDsEmptyLeavesContextUnchanged(t *testing.T) {
	ctx := context.Background()
	if got := WithPreferredAuthIDs(ctx, []string{" ", ""}); got != ctx {
		t.Fatal("WithPreferredAuthIDs() changed the context for empty input")
	}
	if got := WithPreferredAuthIDs(nil, nil); got != nil {
		t.Fatal("WithPreferredAuthIDs() changed a nil context for empty input")
	}
}

func TestPreferredAuthIDsFromContextIsTolerant(t *testing.T) {
	ctx := context.WithValue(context.Background(), preferredAuthIDsContextKey{}, []any{
		" auth-1 ",
		[]byte("auth-2"),
		42,
		"",
	})
	got := preferredAuthIDsFromContext(ctx)
	want := []string{"auth-1", "auth-2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preferredAuthIDsFromContext() = %#v, want %#v", got, want)
	}
}

func TestRequestExecutionMetadataUsesSchedulerPreferredAuthKey(t *testing.T) {
	if coreauth.PreferredAuthIDsMetadataKey != "preferred_auth_ids" {
		t.Fatalf("PreferredAuthIDsMetadataKey = %q, want %q", coreauth.PreferredAuthIDsMetadataKey, "preferred_auth_ids")
	}

	meta := requestExecutionMetadata(WithPreferredAuthIDs(context.Background(), []string{"auth-1"}))
	got, ok := meta[coreauth.PreferredAuthIDsMetadataKey].([]string)
	if !ok {
		t.Fatalf("preferred auth metadata = %#v, want []string", meta[coreauth.PreferredAuthIDsMetadataKey])
	}
	if want := []string{"auth-1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("preferred auth metadata = %#v, want %#v", got, want)
	}
}
