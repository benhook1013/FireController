package reviewexec

import (
	"net/http/httptest"
	"testing"
)

func versionWaitRead(t *testing.T, f *attemptFixture, path, version string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("GET", path, nil)
	request.Header.Set("Authorization", "Bearer "+f.writer)
	request.Header.Set(operationWaitHeader, version)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal("versioned wait read", path, version, response.Code, response.Body.String())
	}
	return response
}
