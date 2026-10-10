package admin

import (
	"net/http"
	"testing"
)

// TestCreateUserRequiresAttachAndKeyActions checks that POST /users cannot
// be used to sidestep admin:UpdateUser (attaching policies) or
// admin:CreateAccessKey (minting keys): previously admin:CreateUser alone
// let a helpdesk role create a user holding the "admin" policy together with
// an access key, even with an explicit Deny on both actions.
func TestCreateUserRequiresAttachAndKeyActions(t *testing.T) {
	e := newEnv(t)
	e.putPolicy("helpdesk", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:CreateUser", "admin:ListUsers"}, "Resource": "*",
	}, map[string]any{
		"Effect": "Deny", "Action": []string{"admin:CreateAccessKey", "admin:UpdateUser", "admin:PutPolicy"}, "Resource": "*",
	})
	e.putPolicy("creator", map[string]any{
		"Effect": "Allow", "Action": []string{"admin:CreateUser", "admin:UpdateUser", "admin:CreateAccessKey"}, "Resource": "*",
	})
	helper := e.addUser("helper", "helpdesk")

	e.do(&helper, "POST", Prefix+"/users", map[string]any{"name": "evil", "policies": []string{"admin"}, "createKey": true}).
		expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&helper, "POST", Prefix+"/users", map[string]any{"name": "evil", "policies": []string{"admin"}}).
		expectError(t, http.StatusForbidden, codeAccessDenied)
	e.do(&helper, "POST", Prefix+"/users", map[string]any{"name": "evil", "createKey": true}).
		expectError(t, http.StatusForbidden, codeAccessDenied)
	// Nothing was created by the refused requests.
	e.root("GET", "/users/evil", nil).expectError(t, http.StatusNotFound, codeNotFound)

	// A plain user (no policies, no key) is still within admin:CreateUser.
	e.do(&helper, "POST", Prefix+"/users", map[string]any{"name": "plain", "policies": []string{}}).
		expectStatus(t, http.StatusCreated)

	// With all three actions allowed the combined request still works.
	creator := e.addUser("creator", "creator")
	r := e.do(&creator, "POST", Prefix+"/users", map[string]any{"name": "full", "policies": []string{"readonly"}, "createKey": true})
	r.expectStatus(t, http.StatusCreated)
	var resp struct {
		User struct {
			Policies []string `json:"policies"`
		} `json:"user"`
		Key *struct {
			AccessKey string `json:"accessKey"`
		} `json:"key"`
	}
	r.json(t, &resp)
	if len(resp.User.Policies) != 1 || resp.User.Policies[0] != "readonly" || resp.Key == nil || resp.Key.AccessKey == "" {
		t.Fatalf("unexpected response %+v", resp)
	}
}
