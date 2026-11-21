package httpapi

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"clusbot/internal/work"
)

func TestTeamTaskHandlerRejectsSuppliedActor(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"action":"claim","expected_version":1,"request_key":"request","actor":"user:somebody-else"}`))
	r.Header.Set("Content-Type", "application/json")
	r.SetPathValue("id", strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	Server{}.teamTaskResponsibility(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client-supplied actor accepted: %d %s", w.Code, w.Body.String())
	}
}

func TestTeamTaskHandlerDoesNotRevealTaskExistence(t *testing.T) {
	missing, denied := httptest.NewRecorder(), httptest.NewRecorder()
	teamTaskResponsibilityError(missing, sql.ErrNoRows)
	teamTaskResponsibilityError(denied, work.ErrAuthorizationDenied)
	if missing.Code != http.StatusNotFound || denied.Code != missing.Code || denied.Body.String() != missing.Body.String() {
		t.Fatalf("access denial reveals existence: missing=%d %q denied=%d %q", missing.Code, missing.Body.String(), denied.Code, denied.Body.String())
	}
}
