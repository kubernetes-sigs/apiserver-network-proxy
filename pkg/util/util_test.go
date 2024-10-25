package util

import (
	"testing"
)

func TestParseLabels(t *testing.T) {
	testCases := []struct {
		input          string
		expectedOutput map[string]string
		shouldError    bool
	}{
		{
			input: "app=myapp,env=prod,version=1.0",
			expectedOutput: map[string]string{
				"app":     "myapp",
				"env":     "prod",
				"version": "1.0",
			},
			shouldError: false,
		},
		{
			input:          "app=myapp,env=prod,invalid",
			expectedOutput: nil,
			shouldError:    true,
		},
		{
			input: "app=myapp",
			expectedOutput: map[string]string{
				"app": "myapp",
			},
			shouldError: false,
		},
		{
			input:          "",
			expectedOutput: map[string]string{},
			shouldError:    true,
		},
		{
			input: " key = value , another = test ",
			expectedOutput: map[string]string{
				"key":     "value",
				"another": "test",
			},
			shouldError: false,
		},
	}

	for _, tc := range testCases {
		output, err := ParseLabels(tc.input)

		// Check for unexpected errors or missing errors
		if tc.shouldError && err == nil {
			t.Errorf("expected error for input %q but got none", tc.input)
			continue
		}
		if !tc.shouldError && err != nil {
			t.Errorf("did not expect error for input %q but got: %v", tc.input, err)
			continue
		}

		// Compare maps if there was no error
		if !tc.shouldError {
			if len(output) != len(tc.expectedOutput) {
				t.Errorf("for input %q, expected map length %d but got %d", tc.input, len(tc.expectedOutput), len(output))
			}
			for key, expectedValue := range tc.expectedOutput {
				if output[key] != expectedValue {
					t.Errorf("for input %q, expected %q=%q but got %q=%q", tc.input, key, expectedValue, key, output[key])
				}
			}
		}
	}
}
