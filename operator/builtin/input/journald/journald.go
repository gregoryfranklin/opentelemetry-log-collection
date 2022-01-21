// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux
// +build linux

package journald

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	jsoniter "github.com/json-iterator/go"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-log-collection/entry"
	"github.com/open-telemetry/opentelemetry-log-collection/operator"
	"github.com/open-telemetry/opentelemetry-log-collection/operator/helper"
)

func init() {
	operator.Register("journald_input", func() operator.Builder { return NewJournaldInputConfig("") })
}

func NewJournaldInputConfig(operatorID string) *JournaldInputConfig {
	return &JournaldInputConfig{
		InputConfig: helper.NewInputConfig(operatorID, "journald_input"),
		StartAt:     "end",
		Priority:    "info",
	}
}

// JournaldInputConfig is the configuration of a journald input operator
type JournaldInputConfig struct {
	helper.InputConfig `mapstructure:",squash" yaml:",inline"`

	Directory *string  `mapstructure:"directory,omitempty" json:"directory,omitempty" yaml:"directory,omitempty"`
	Files     []string `mapstructure:"files,omitempty"     json:"files,omitempty"     yaml:"files,omitempty"`
	StartAt   string   `mapstructure:"start_at,omitempty"  json:"start_at,omitempty"  yaml:"start_at,omitempty"`
	Units     []string `mapstructure:"units,omitempty"     json:"units,omitempty"     yaml:"units,omitempty"`
	Priority  string   `mapstructure:"priority,omitempty"  json:"priority,omitempty"  yaml:"priority,omitempty"`
}

// Build will build a journald input operator from the supplied configuration
func (c JournaldInputConfig) Build(buildContext operator.BuildContext) ([]operator.Operator, error) {
	inputOperator, err := c.InputConfig.Build(buildContext)
	if err != nil {
		return nil, err
	}

	args := make([]string, 0, 10)

	// Export logs in UTC time
	args = append(args, "--utc")

	// Export logs as JSON
	args = append(args, "--output=json")

	// Continue watching logs until cancelled
	args = append(args, "--follow")

	switch c.StartAt {
	case "end":
	case "beginning":
		args = append(args, "--no-tail")
	default:
		return nil, fmt.Errorf("invalid value '%s' for parameter 'start_at'", c.StartAt)
	}

	for _, unit := range c.Units {
		args = append(args, "--unit", unit)
	}

	args = append(args, "--priority", c.Priority)

	switch {
	case c.Directory != nil:
		args = append(args, "--directory", *c.Directory)
	case len(c.Files) > 0:
		for _, file := range c.Files {
			args = append(args, "--file", file)
		}
	}

	journaldInput := &JournaldInput{
		InputOperator: inputOperator,
		newCmd: func(ctx context.Context, cursor []byte) cmd {
			if cursor != nil {
				args = append(args, "--after-cursor", string(cursor))
			}
			return exec.CommandContext(ctx, "journalctl", args...) // #nosec - ...
			// journalctl is an executable that is required for this operator to function
		},
		json: jsoniter.ConfigFastest,
	}
	return []operator.Operator{journaldInput}, nil
}

// JournaldInput is an operator that process logs using journald
type JournaldInput struct {
	helper.InputOperator

	newCmd func(ctx context.Context, cursor []byte) cmd

	persister operator.Persister
	json      jsoniter.API
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

type cmd interface {
	StdoutPipe() (io.ReadCloser, error)
	Start() error
}

var lastReadCursorKey = "lastReadCursor"

// Start will start generating log entries.
func (operator *JournaldInput) Start(persister operator.Persister) error {
	ctx, cancel := context.WithCancel(context.Background())
	operator.cancel = cancel

	// Start from a cursor if there is a saved offset
	cursor, err := persister.Get(ctx, lastReadCursorKey)
	if err != nil {
		return fmt.Errorf("failed to get journalctl state: %s", err)
	}

	operator.persister = persister

	// Start journalctl
	cmd := operator.newCmd(ctx, cursor)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to get journalctl stdout: %s", err)
	}
	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start journalctl: %s", err)
	}

	// Start the reader goroutine
	operator.wg.Add(1)
	go func() {
		defer operator.wg.Done()

		stdoutBuf := bufio.NewReader(stdout)

		for {
			line, err := stdoutBuf.ReadBytes('\n')
			if err != nil {
				if err != io.EOF {
					operator.Errorw("Received error reading from journalctl stdout", zap.Error(err))
				}
				return
			}

			entry, cursor, err := operator.parseJournalEntry(line)
			if err != nil {
				operator.Warnw("Failed to parse journal entry", zap.Error(err))
				continue
			}
			if err := operator.persister.Set(ctx, lastReadCursorKey, []byte(cursor)); err != nil {
				operator.Warnw("Failed to set offset", zap.Error(err))
			}
			operator.Write(ctx, entry)
		}
	}()

	return nil
}

func (operator *JournaldInput) parseJournalEntry(line []byte) (*entry.Entry, string, error) {
	var body map[string]interface{}
	err := operator.json.Unmarshal(line, &body)
	if err != nil {
		return nil, "", err
	}

	timestamp, ok := body["__REALTIME_TIMESTAMP"]
	if !ok {
		return nil, "", errors.New("journald body missing __REALTIME_TIMESTAMP field")
	}

	timestampString, ok := timestamp.(string)
	if !ok {
		return nil, "", errors.New("journald field for timestamp is not a string")
	}

	timestampInt, err := strconv.ParseInt(timestampString, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("parse timestamp: %s", err)
	}

	delete(body, "__REALTIME_TIMESTAMP")

	cursor, ok := body["__CURSOR"]
	if !ok {
		return nil, "", errors.New("journald body missing __CURSOR field")
	}

	cursorString, ok := cursor.(string)
	if !ok {
		return nil, "", errors.New("journald field for cursor is not a string")
	}

	msg, ok := body["MESSAGE"]
	if !ok {
		return nil, "", errors.New("journald body missing MESSAGE field")
	}
	delete(body, "MESSAGE")

	entry, err := operator.NewEntry(msg)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create entry: %s", err)
	}
	entry.Timestamp = time.Unix(0, timestampInt*1000) // in microseconds

	for k, v := range body {
		switch k {
		case "PRIORITY":
			if err := addSeverity(entry, v); err != nil {
				return nil, "", err
			}
		default:
			if val := convertField(v); val != "" {
				entry.AddAttribute(k, val)
			}
		}
	}

	return entry, cursorString, nil
}

// Stop will stop generating logs.
func (operator *JournaldInput) Stop() error {
	operator.cancel()
	operator.wg.Wait()
	return nil
}

var severityMapping = [...]entry.Severity{
	0: entry.Fatal,
	1: entry.Error3,
	2: entry.Error2,
	3: entry.Error,
	4: entry.Warn,
	5: entry.Info2,
	6: entry.Info,
	7: entry.Debug,
}

var severityText = [...]string{
	0: "emerg",
	1: "alert",
	2: "crit",
	3: "err",
	4: "warning",
	5: "notice",
	6: "info",
	7: "debug",
}

func addSeverity(e *entry.Entry, sev interface{}) error {
	sevInt, err := strconv.Atoi(sev.(string))
	if err != nil {
		return fmt.Errorf("severity field is not an int")
	}

	if sevInt < 0 || sevInt > 7 {
		return fmt.Errorf("invalid severity '%d'", sevInt)
	}

	e.Severity = severityMapping[sevInt]
	e.SeverityText = severityText[sevInt]
	return nil
}

func convertField(val interface{}) string {
	// attributes only supports strings at the moment
	// in future, these should return AttributeValue types
	// https://github.com/open-telemetry/opentelemetry-log-collection/issues/190
	switch v := val.(type) {
	case []byte:
		return base64.StdEncoding.EncodeToString(v)
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", val)
	}
}
