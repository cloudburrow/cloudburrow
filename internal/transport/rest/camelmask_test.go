package rest

import "testing"

func TestCamelMask(t *testing.T) {
	for in, want := range map[string]string{
		"labels,memory_size_gb":  "labels,memorySizeGb",
		"redis_configs":          "redisConfigs",
		"displayName,a_b.c_d":    "displayName,aB.cD",
		"retryConfig.retryCount": "retryConfig.retryCount",
	} {
		if got := camelMask(in); got != want {
			t.Errorf("camelMask(%q) = %q, want %q", in, got, want)
		}
	}
}
