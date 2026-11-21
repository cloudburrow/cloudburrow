package main

import (
	"fmt"
	"sort"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/ratequota"
	"github.com/cloudburrow/cloudburrow/internal/service/parametermanager"
)

// newRequestQuotas is the instance's per-minute request quotas (#2063):
// each enabled service's documented metrics, with the limits the
// configuration's requestQuotas sets for every project. A metric no
// enabled service enforces is refused, so a misspelt one is not ignored.
func newRequestQuotas(cfg config.Config) (*ratequota.Limiter, error) {
	l := ratequota.New()
	if parameterManagerEnabled(cfg) {
		l.Register(parametermanager.Quotas()...)
	}
	metrics := make([]string, 0, len(cfg.RequestQuotas))
	for m := range cfg.RequestQuotas {
		metrics = append(metrics, m)
	}
	sort.Strings(metrics)
	for _, m := range metrics {
		if err := l.Set(m, "", cfg.RequestQuotas[m]); err != nil {
			return nil, fmt.Errorf("requestQuotas: %w (the metrics enforced are those of the enabled services' quotas, such as %s)", err, parametermanager.Service+"/read_requests")
		}
	}
	return l, nil
}
