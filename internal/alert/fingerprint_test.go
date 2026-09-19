package alert

import "testing"

func TestFingerprintIgnoresMapOrdering(t *testing.T) {
	a := map[string]string{"alertname": "HighP99", "ServiceName": "checkout", "team": "payments"}
	b := map[string]string{"team": "payments", "alertname": "HighP99", "ServiceName": "checkout"}

	if fingerprint(a) != fingerprint(b) {
		t.Errorf("same labels hashed differently: %d vs %d", fingerprint(a), fingerprint(b))
	}
}

func TestFingerprintIsStableAcrossCalls(t *testing.T) {
	labels := map[string]string{"alertname": "HighP99", "ServiceName": "checkout"}

	first := fingerprint(labels)
	for i := 0; i < 100; i++ {
		if got := fingerprint(labels); got != first {
			t.Fatalf("call %d returned %d, want %d", i, got, first)
		}
	}
}

func TestFingerprintDistinguishesLabelSets(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]string
	}{
		{
			name: "different value",
			a:    map[string]string{"ServiceName": "checkout"},
			b:    map[string]string{"ServiceName": "cart"},
		},
		{
			name: "different key",
			a:    map[string]string{"ServiceName": "checkout"},
			b:    map[string]string{"HostName": "checkout"},
		},
		{
			name: "extra label",
			a:    map[string]string{"ServiceName": "checkout"},
			b:    map[string]string{"ServiceName": "checkout", "team": "payments"},
		},
		{
			// A naive "key=value" join collides on these two, which would
			// merge two genuinely different alerts into one instance.
			name: "separator appears inside a key or value",
			a:    map[string]string{"a": "b=c"},
			b:    map[string]string{"a=b": "c"},
		},
		{
			// Same risk for the separator between pairs.
			name: "pair separator appears inside a value",
			a:    map[string]string{"a": "b", "c": "d"},
			b:    map[string]string{"a": "b\x00c=d"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if fingerprint(tc.a) == fingerprint(tc.b) {
				t.Errorf("distinct label sets share fingerprint %d:\n a: %v\n b: %v",
					fingerprint(tc.a), tc.a, tc.b)
			}
		})
	}
}

func TestFingerprintEmptyAndNil(t *testing.T) {
	if fingerprint(nil) != fingerprint(map[string]string{}) {
		t.Error("nil and empty label sets must hash alike")
	}
}
