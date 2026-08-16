// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func main() {
	pulumi.Run(func(*pulumi.Context) error {
		if os.Getenv("LIFTR_FORBIDDEN_AMBIENT_VALUE") != "" {
			return fmt.Errorf("ambient Liftr environment reached the Pulumi program")
		}
		path := os.Getenv("LIFTR_INPUT_FILE")
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read Liftr input: %w", err)
		}
		var input struct {
			NonSecret bool `json:"nonSecret"`
		}
		if err := json.Unmarshal(contents, &input); err != nil {
			return fmt.Errorf("decode Liftr input: %w", err)
		}
		if !input.NonSecret {
			return fmt.Errorf("non-secret test input is required")
		}
		return nil
	})
}
