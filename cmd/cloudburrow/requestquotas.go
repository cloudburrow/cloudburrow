package main

import (
	"fmt"
	"sort"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/ratequota"
	"github.com/cloudburrow/cloudburrow/internal/service/cloudquotas"
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

// requestQuotaInfos makes the request quotas Cloud Quotas quotas (#2090):
// each metric a QuotaInfo of its service, quota ID its limit's name, at
// the value the configuration or /admin/quotas sets for the project, so a
// QuotaPreference for it changes the limit its calls are charged against.
func requestQuotaInfos(l *ratequota.Limiter) cloudquotas.Provider {
	return func(project string) []cloudquotas.Quota {
		ms := l.Metrics()
		out := make([]cloudquotas.Quota, 0, len(ms))
		for _, m := range ms {
			v, _ := l.Configured(m.Name, project)
			out = append(out, cloudquotas.Quota{
				Service: m.Service, QuotaID: m.QuotaID(), Metric: m.Name, MetricDisplayName: m.Title,
				QuotaDisplayName: m.LimitTitle, MetricUnit: "1/min/{project}", Default: v, RefreshInterval: "minute",
				Enforced: true, Source: m.Source,
			})
		}
		return out
	}
}
