package main

import (
	"reflect"
	"testing"
)

func ids(items []*Item) []int {
	var out []int
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func TestSelectWork(t *testing.T) {
	items := []*Item{
		{ID: 1, State: statePending, Profile: strPtr("iida")},
		{ID: 2, State: stateResearching, Profile: strPtr("saitama")},
		{ID: 3, State: statePending, Profile: strPtr("saitama")},
		{ID: 4, State: stateDone},
		{ID: 5, State: statePending, Profile: nil},
		{ID: 6, State: stateFailed, Profile: strPtr("iida")},
	}
	cases := []struct {
		name        string
		profile     string
		wantActive  []int
		wantPending []int
	}{
		{name: "all profiles", wantActive: []int{2}, wantPending: []int{1, 3, 5}},
		{name: "single profile", profile: "saitama", wantActive: []int{2}, wantPending: []int{3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := config{Profiles: []string{"iida", "saitama"}, Profile: tc.profile}
			active, pending := selectWork(&Queue{Items: items}, c)
			if got := ids(active); !reflect.DeepEqual(got, tc.wantActive) {
				t.Errorf("active = %v, want %v", got, tc.wantActive)
			}
			if got := ids(pending); !reflect.DeepEqual(got, tc.wantPending) {
				t.Errorf("pending = %v, want %v", got, tc.wantPending)
			}
		})
	}
}

func TestChooseProfile(t *testing.T) {
	c := config{Profiles: []string{"iida", "saitama"}}
	cases := []struct {
		name  string
		items []*Item
		want  string
	}{
		{name: "empty queue prefers first", want: "iida"},
		{
			name:  "assign least loaded",
			items: []*Item{{ID: 1, Profile: strPtr("iida")}, {ID: 2, Profile: strPtr("iida")}},
			want:  "saitama",
		},
		{
			name:  "round robin fallback for old items",
			items: []*Item{{ID: 1, Profile: nil}, {ID: 2, Profile: nil}, {ID: 3, Profile: nil}},
			want:  "saitama",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.chooseProfile(&Queue{Items: tc.items}); got != tc.want {
				t.Fatalf("chooseProfile = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseResearchLines(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		want    researchOutcome
		wantErr bool
	}{
		{
			name: "json lines and plain import log",
			output: "{\"type\":\"progress\",\"status\":\"running\"}\n" +
				"{\"type\":\"complete\",\"report\":\"# 調査\",\"sources\":[{\"title\":\"a\"},{\"title\":\"b\"}],\"imported_count\":2}\n" +
				"imported 2 sources\n",
			want: researchOutcome{Report: "# 調査", Sources: 2, Imported: intPtr(2)},
		},
		{
			name:    "failed status",
			output:  "{\"type\":\"complete\",\"status\":\"failed\",\"message\":\"boom\"}\n",
			wantErr: true,
		},
		{
			name:    "no complete event",
			output:  "imported 1 sources\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseResearchLines(tc.output)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseResearchLines error = %v, wantErr %t", err, tc.wantErr)
			}
			if err == nil {
				if got.Report != tc.want.Report || got.Sources != tc.want.Sources {
					t.Fatalf("outcome = %+v, want %+v", got, tc.want)
				}
				if (got.Imported == nil) != (tc.want.Imported == nil) ||
					(got.Imported != nil && *got.Imported != *tc.want.Imported) {
					t.Fatalf("imported = %v, want %v", got.Imported, tc.want.Imported)
				}
			}
		})
	}
}

func TestQuotaProblem(t *testing.T) {
	cases := []struct {
		name    string
		rolling *float64
		weekly  *float64
		wantLow bool
	}{
		{name: "enough", rolling: floatPtr(80), weekly: floatPtr(60)},
		{name: "rolling low", rolling: floatPtr(5), weekly: floatPtr(60), wantLow: true},
		{name: "weekly low", rolling: floatPtr(80), weekly: floatPtr(2), wantLow: true},
		{name: "unknown quotas are not below threshold"},
	}
	c := config{Reserve: 10, MinRolling: 10}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problem := quotaProblem(&usageInfo{Rolling: tc.rolling, Weekly: tc.weekly}, c)
			if (problem != "") != tc.wantLow {
				t.Fatalf("quotaProblem = %q, wantLow %t", problem, tc.wantLow)
			}
		})
	}
}

func floatPtr(v float64) *float64 { return &v }
