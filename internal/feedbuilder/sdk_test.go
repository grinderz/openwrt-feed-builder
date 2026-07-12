package feedbuilder

import "testing"

func TestSDKMatrixDefaultsFromGlobalConfig(t *testing.T) {
	cfg := &Config{
		Layout:  Layout{Versions: []string{"24.10.6", "24.10.7"}},
		Targets: []string{"ramips/mt7621", "mediatek/filogic"},
	}
	got, err := sdkMatrix(cfg, Source{"type": "sdk"})
	if err != nil {
		t.Fatal(err)
	}
	want := []sdkBuild{
		{"24.10.6", "ramips", "mt7621"},
		{"24.10.6", "mediatek", "filogic"},
		{"24.10.7", "ramips", "mt7621"},
		{"24.10.7", "mediatek", "filogic"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: got %v, want %v", i, got[i], want[i])
		}
	}
}

func TestSDKMatrixPerSourceOverride(t *testing.T) {
	cfg := &Config{
		Layout:  Layout{Versions: []string{"24.10.6", "24.10.7"}},
		Targets: []string{"ramips/mt7621"},
	}
	src := Source{
		"type":     "sdk",
		"releases": []any{"24.10.7"},
		"targets":  []any{"mediatek/filogic"},
	}
	got, err := sdkMatrix(cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (sdkBuild{"24.10.7", "mediatek", "filogic"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSDKMatrixRejectsPartialTarget(t *testing.T) {
	cfg := &Config{
		Layout:  Layout{Versions: []string{"24.10.7"}},
		Targets: []string{"mt7621"}, // valid as a global filter, unusable for the SDK
	}
	if _, err := sdkMatrix(cfg, Source{"type": "sdk"}); err == nil {
		t.Fatal("expected an error for a target without a subtarget")
	}
}
