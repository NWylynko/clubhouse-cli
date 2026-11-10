package controlplane

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	BudgetUnit = "concurrent boxes"

	BudgetEnv = "CLUB_BUDGET_CEILING"

	BudgetDefaultBoxConcurrent   = 8
	BudgetDefaultHeldConnections = 256

	HeldConnectionsPerBoxSlot = BudgetDefaultHeldConnections / BudgetDefaultBoxConcurrent
)

type Budget struct {
	Ceiling         int
	Unset           bool
	BoxConcurrent   int
	HeldConnections int
}

func ResolveBudget(ceiling int, configured bool) (Budget, error) {
	if configured && ceiling < 0 {
		return Budget{}, fmt.Errorf("%s must be >= 0 (got %d)", BudgetEnv, ceiling)
	}
	if !configured {
		return Budget{
			Unset:           true,
			BoxConcurrent:   BudgetDefaultBoxConcurrent,
			HeldConnections: BudgetDefaultHeldConnections,
		}, nil
	}
	return Budget{
		Ceiling:         ceiling,
		BoxConcurrent:   ceiling,
		HeldConnections: ceiling * HeldConnectionsPerBoxSlot,
	}, nil
}

func ParseBudgetEnv(v string) (Budget, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return Budget{}, fmt.Errorf("%s is unset — set it to max concurrent managed boxes", BudgetEnv)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return Budget{}, fmt.Errorf("%s: %w", BudgetEnv, err)
	}
	return ResolveBudget(n, true)
}

func (b Budget) LogLine() string {
	if b.Unset {
		return fmt.Sprintf("budget ceiling unset — today's defaults (%s): box concurrent %d, held connections %d",
			BudgetUnit, b.BoxConcurrent, b.HeldConnections)
	}
	return fmt.Sprintf("budget ceiling %d %s → box concurrent %d, held connections %d (×%d per box slot)",
		b.Ceiling, BudgetUnit, b.BoxConcurrent, b.HeldConnections, HeldConnectionsPerBoxSlot)
}
