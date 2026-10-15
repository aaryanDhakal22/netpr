package printer

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"

	ipp "github.com/phin1x/go-ipp"
)

// Active is the printer currently in use. The printer page can switch it at
// runtime; holders call Get for every operation instead of keeping a Client.
type Active struct{ p atomic.Pointer[Client] }

func NewActive(c *Client) *Active {
	a := &Active{}
	a.p.Store(c)
	return a
}

func (a *Active) Get() *Client  { return a.p.Load() }
func (a *Active) Set(c *Client) { a.p.Store(c) }

// ParseAddress turns what someone types (an IP, a host:port or a full URI)
// into a printer URI. A bare address gets the ipp scheme and the /ipp/print
// path that AirPrint printers use.
func ParseAddress(in string) (string, error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", errors.New("enter the printer's IP address or ipp:// URI")
	}
	if !strings.Contains(in, "://") {
		in = "ipp://" + in
	}
	u, err := url.Parse(in)
	if err != nil {
		return "", fmt.Errorf("not a valid address: %w", err)
	}
	if u.Scheme != "ipp" && u.Scheme != "ipps" {
		return "", errors.New("the address must use ipp:// or ipps://")
	}
	if u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("expected an address like 192.168.1.50 or ipp://192.168.1.50/ipp/print")
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/ipp/print"
	}
	return u.String(), nil
}

// QueuedJob is a job the printer itself still holds, from any client.
type QueuedJob struct {
	ID      int
	Name    string
	User    string
	State   string
	Reasons []string
}

// Jobs lists the printer's unfinished jobs with Get-Jobs.
func (c *Client) Jobs(ctx context.Context) ([]QueuedJob, error) {
	req := c.newRequest(ipp.OperationGetJobs, "")
	req.OperationAttributes[ipp.AttributeWhichJobs] = "not-completed"
	req.OperationAttributes[ipp.AttributeRequestedAttributes] = []string{
		ipp.AttributeJobID, ipp.AttributeJobName, ipp.AttributeJobOriginatingUserName,
		ipp.AttributeJobState, ipp.AttributeJobStateReasons,
	}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	var out []QueuedJob
	for _, a := range resp.JobAttributes {
		j := QueuedJob{
			ID:      intVal(a, ipp.AttributeJobID, 0),
			Name:    stringVal(a, ipp.AttributeJobName),
			User:    stringVal(a, ipp.AttributeJobOriginatingUserName),
			State:   jobStateName(intVal(a, ipp.AttributeJobState, 0)),
			Reasons: stringVals(a, ipp.AttributeJobStateReasons),
		}
		if j.ID != 0 {
			out = append(out, j)
		}
	}
	return out, nil
}

// CancelJob cancels a job on the printer. Printers without authentication
// only let the submitting user cancel, so user should be the job's owner.
func (c *Client) CancelJob(ctx context.Context, jobID int, user string) error {
	req := c.newRequest(ipp.OperationCancelJob, user)
	req.OperationAttributes[ipp.AttributeJobID] = jobID
	_, err := c.do(ctx, req)
	if e := (*IPPError)(nil); errors.As(err, &e) && e.Status == ipp.StatusErrorNotFound {
		return ErrJobNotFound
	}
	return err
}

// RawAttribute is one printer attribute formatted for display.
type RawAttribute struct {
	Name  string
	Value string
}

// Attributes returns every printer attribute the printer reports, for
// troubleshooting.
func (c *Client) Attributes(ctx context.Context) ([]RawAttribute, error) {
	req := c.newRequest(ipp.OperationGetPrinterAttributes, "")
	req.OperationAttributes[ipp.AttributeRequestedAttributes] = []string{"all"}
	resp, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.PrinterAttributes) == 0 {
		return nil, errors.New("printer returned no attributes")
	}
	a := resp.PrinterAttributes[0]
	out := make([]RawAttribute, 0, len(a))
	for _, name := range slices.Sorted(maps.Keys(a)) {
		out = append(out, RawAttribute{Name: name, Value: formatValues(a[name])})
	}
	return out, nil
}

func formatValues(vals []ipp.Attribute) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = formatValue(v.Value)
	}
	return strings.Join(parts, ", ")
}

func formatValue(v any) string {
	switch v := v.(type) {
	case ipp.Collection:
		var b strings.Builder
		b.WriteString("{")
		for i, name := range slices.Sorted(maps.Keys(v)) {
			if i > 0 {
				b.WriteString(" ")
			}
			b.WriteString(name + "=" + formatValues(v[name]))
		}
		b.WriteString("}")
		return b.String()
	case string:
		return strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f || r == '�' {
				return -1
			}
			return r
		}, v)
	}
	return fmt.Sprint(v)
}
