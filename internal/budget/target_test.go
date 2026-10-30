package budget

import "testing"

func TestAlvoTrabalho(t *testing.T) {
	for _, tc := range []struct {
		name           string
		window, manual int
		want           int
	}{
		{"1M catalog window is capped at the default target", 922_000, 0, AlvoTrabalhoPadrao},
		{"200k window keeps room for the round in flight", 200_000, 0, 160_000},
		{"no known window still gets a working target", 0, 0, AlvoTrabalhoPadrao},
		{"manual target below the ceiling is honoured", 922_000, 120_000, 120_000},
		{"manual target above the ceiling is clamped", 200_000, 500_000, 160_000},
		{"negative manual target switches the ladder off", 922_000, -1, 0},
		{"manual target without a window is honoured", 0, 90_000, 90_000},
		{"window smaller than the reserves has no target", 10_000, 0, 0},
	} {
		if got := AlvoTrabalho(tc.window, tc.manual); got != tc.want {
			t.Errorf("%s: AlvoTrabalho(%d,%d) = %d; want %d", tc.name, tc.window, tc.manual, got, tc.want)
		}
	}
}

func TestLadderOrder(t *testing.T) {
	if !(DegrauAviso < DegrauPoda && DegrauPoda <= DegrauExige && DestinoPoda < DegrauAviso) {
		t.Fatalf("ladder out of order: aviso=%d poda=%d exige=%d destino=%d", DegrauAviso, DegrauPoda, DegrauExige, DestinoPoda)
	}
}
