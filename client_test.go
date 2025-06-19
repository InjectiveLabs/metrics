package metrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBaseTags(t *testing.T) {
	tests := []struct {
		name     string
		config   *StatterConfig
		expected []string
	}{
		{
			name: "Datadog agent with default tags",
			config: &StatterConfig{
				Agent:    DatadogAgent,
				EnvName:  "prod",
				HostName: "host1",
				DefaultTags: []interface{}{
					"key1", "value1",
					"key2", "value2",
				},
			},
			expected: []string{"env:prod", "machine:host1", "key1:value1", "key2:value2"},
		},
		{
			name: "Telegraf agent with default tags",
			config: &StatterConfig{
				Agent:    TelegrafAgent,
				EnvName:  "dev",
				HostName: "host2",
				DefaultTags: []interface{}{
					"key1", "value1",
					"key2", "value2",
				},
			},
			expected: []string{"env", "dev", "machine", "host2", "key1", "value1", "key2", "value2"},
		},
		{
			name: "Datadog agent with no default tags",
			config: &StatterConfig{
				Agent:       DatadogAgent,
				EnvName:     "staging",
				HostName:    "host3",
				DefaultTags: nil,
			},
			expected: []string{"env:staging", "machine:host3"},
		},
		{
			name: "Telegraf agent with no default tags",
			config: &StatterConfig{
				Agent:       TelegrafAgent,
				EnvName:     "test",
				HostName:    "host4",
				DefaultTags: nil,
			},
			expected: []string{"env", "test", "machine", "host4"},
		},
		{
			name: "Empty config",
			config: &StatterConfig{
				Agent:       DatadogAgent,
				DefaultTags: nil,
			},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Cleanup(func() {
				config = nil
			})
			config = tt.config
			result := tt.config.BaseTags()
			assert.ElementsMatch(t, tt.expected, result)
		})
	}
}
