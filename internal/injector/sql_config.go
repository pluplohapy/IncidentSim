package injector

import (
	"context"
	"fmt"
	"strings"
)

// SQLConfigInjector injects a database-level fault by running statements
// against the target service at session start.
//
// It is the injector behind db-level tasks: a raised connection limit, a
// disabled autovacuum, a bloated table. The statements run through the runtime
// adapter, so the same injector works on compose and on a microVM.
type SQLConfigInjector struct{}

// NewSQLConfigInjector returns the database-level fault injector.
func NewSQLConfigInjector() *SQLConfigInjector { return &SQLConfigInjector{} }

// Kind implements Injector.
func (i *SQLConfigInjector) Kind() string { return KindSQLConfig }

// Activate implements Injector.
func (i *SQLConfigInjector) Activate(ctx context.Context, env Environment, fault Fault) error {
	statements := NewOptions(fault.Options).StringSlice("statements")
	if len(statements) == 0 {
		return fmt.Errorf("%w: sql_config fault on %q declares no statements", ErrBadOptions, fault.Target)
	}
	usable := 0
	for _, statement := range statements {
		if strings.TrimSpace(statement) != "" {
			usable++
		}
	}
	if usable == 0 {
		return fmt.Errorf("%w: sql_config fault on %q declares no statements", ErrBadOptions, fault.Target)
	}

	for index, statement := range statements {
		trimmed := strings.TrimSpace(statement)
		if trimmed == "" {
			continue
		}
		result, err := env.ExecSQL(ctx, fault.Target, trimmed)
		if err != nil {
			return fmt.Errorf("statement %d on %s: %w", index, fault.Target, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("statement %d on %s exited %d: %s", index, fault.Target, result.ExitCode, strings.TrimSpace(result.Stderr))
		}
	}
	return nil
}
