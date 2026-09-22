package feedbuilder

import "testing"

func TestSDKMatrixDefaultsFromGlobalConfig(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Layout:  Layout{Versions: []string{tRel24106, tRel24107}},
		Targets: []string{tTargetMT7621, tTargetFilogic},
	}

	got, err := sdkMatrix(cfg, Source{tKeyType: tTypeSDK})
	if err != nil {
		t.Fatal(err)
	}

	want := []sdkBuild{
		{tRel24106, "ramips", tSubtargetMT7621},
		{tRel24106, tTargetMediatek, tSubtargetFilogic},
		{tRel24107, "ramips", tSubtargetMT7621},
		{tRel24107, tTargetMediatek, tSubtargetFilogic},
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
	t.Parallel()

	cfg := &Config{
		Layout:  Layout{Versions: []string{tRel24106, tRel24107}},
		Targets: []string{tTargetMT7621},
	}
	src := Source{
		tKeyType:   tTypeSDK,
		"releases": []any{tRel24107},
		"targets":  []any{tTargetFilogic},
	}

	got, err := sdkMatrix(cfg, src)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0] != (sdkBuild{tRel24107, tTargetMediatek, tSubtargetFilogic}) {
		t.Fatalf("got %v", got)
	}
}

func TestSDKMatrixRejectsPartialTarget(t *testing.T) {
	t.Parallel()

	cfg := &Config{
		Layout:  Layout{Versions: []string{tRel24107}},
		Targets: []string{tSubtargetMT7621}, // valid as a global filter, unusable for the SDK
	}
	if _, err := sdkMatrix(cfg, Source{tKeyType: tTypeSDK}); err == nil {
		t.Fatal("expected an error for a target without a subtarget")
	}
}
