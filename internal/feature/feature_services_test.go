package feature

import (
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

func TestUpsertServiceReplacesInPlace(t *testing.T) {
	list := []state.Service{
		{Name: "web", Unit: "old-web"},
		{Name: "jobs", Unit: "jobs"},
	}
	got := upsertService(list, state.Service{Name: "web", Unit: "new-web"}, true)
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(got), got)
	}
	var web *state.Service
	for i := range got {
		if got[i].Name == "web" {
			web = &got[i]
		}
	}
	if web == nil || web.Unit != "new-web" {
		t.Errorf("web not replaced: %+v", got)
	}
}

func TestUpsertServiceDropsFailedOptional(t *testing.T) {
	list := []state.Service{{Name: "web"}, {Name: "css", Optional: true}}
	// An optional service that did not come back must leave state, or status
	// would keep claiming it is running.
	got := upsertService(list, state.Service{Name: "css", Optional: true}, false)
	if len(got) != 1 || got[0].Name != "web" {
		t.Errorf("failed optional service still recorded: %+v", got)
	}
}

func TestUpsertServiceAddsWhenAbsent(t *testing.T) {
	got := upsertService(nil, state.Service{Name: "web"}, true)
	if len(got) != 1 || got[0].Name != "web" {
		t.Errorf("service not added: %+v", got)
	}
}

func TestServiceNamesReportsEmpty(t *testing.T) {
	if got := serviceNames(&manifest.Manifest{}); len(got) != 1 || got[0] != "(none declared)" {
		t.Errorf("serviceNames on empty manifest = %v", got)
	}
	m := &manifest.Manifest{Services: []manifest.Service{{Name: "web"}, {Name: "jobs"}}}
	got := serviceNames(m)
	if len(got) != 2 || got[0] != "web" || got[1] != "jobs" {
		t.Errorf("serviceNames = %v", got)
	}
}
