package core

import "testing"

func TestDatabaseFileURIPreservesPathWithoutAuthority(t *testing.T) {
	for _, test := range []struct {
		path string
		want string
	}{
		{"/private/controller/firecontroller.sqlite", "file:///private/controller/firecontroller.sqlite"},
		{"C:/Users/operator/controller/firecontroller.sqlite", "file:///C:/Users/operator/controller/firecontroller.sqlite"},
		{"D:/private files/controller#1%/firecontroller.sqlite", "file:///D:/private%20files/controller%231%25/firecontroller.sqlite"},
		{"/private/controller?scope=other/firecontroller.sqlite", "file:///private/controller%3Fscope=other/firecontroller.sqlite"},
	} {
		t.Run(test.path, func(t *testing.T) {
			uri := databaseURL(test.path)
			if uri.String() != test.want || uri.Host != "" || uri.RawQuery != "" || uri.Fragment != "" {
				t.Fatalf("database path changed URI meaning: %s", uri.String())
			}
		})
	}
}
