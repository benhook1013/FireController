package client

import (
	"bufio"
	"context"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	MessageWaitDefaultLimit   = 20
	MessageWaitDefaultSeconds = 25
	MessageWaitMaxLimit       = 100
	MessageWaitMaxSeconds     = 60
	messageWaitFrameBytes     = 1 << 20
)

// ValidateMessageWait validates explicit wait arguments. Adapters call this
// before credentials or network access; typed-client zero defaults are normalized
// separately before applying the same strict bounds.
func ValidateMessageWait(cursor string, limit, waitSeconds int) error {
	if !validOpaqueCursor(cursor) || limit < 1 || limit > MessageWaitMaxLimit || waitSeconds < 1 || waitSeconds > MessageWaitMaxSeconds {
		return validation()
	}
	return nil
}

// MessageWaitBudget returns the total budget for validated explicit wait seconds.
func MessageWaitBudget(waitSeconds int) time.Duration {
	return time.Duration(waitSeconds)*time.Second + requestTimeout
}

var errMessageWaitExpired = errors.New("message wait expired")

// WaitForMessages owns one incoming-message stream until the first page with
// message.sent events or a quiet timeout. It neither acknowledges messages nor
// reconnects; the caller retains its cursor if any error occurs. Typed zero
// limit/waitSeconds select 20 events and 25 seconds respectively.
func (c *Client) WaitForMessages(ctx context.Context, cursor string, limit, waitSeconds int) (MessageEventPage, error) {
	if limit == 0 {
		limit = MessageWaitDefaultLimit
	}
	if waitSeconds == 0 {
		waitSeconds = MessageWaitDefaultSeconds
	}
	if err := ValidateMessageWait(cursor, limit, waitSeconds); err != nil {
		return MessageEventPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return MessageEventPage{}, contextError(err)
	}
	outer, cancelOuter := context.WithTimeout(ctx, MessageWaitBudget(waitSeconds))
	defer cancelOuter()
	stream, cancelStream := context.WithCancelCause(outer)
	defer cancelStream(context.Canceled)
	readyTimer := time.AfterFunc(requestTimeout, func() { cancelStream(context.DeadlineExceeded) })
	defer readyTimer.Stop()
	var waitTimer *time.Timer
	defer func() {
		if waitTimer != nil {
			waitTimer.Stop()
		}
	}()
	ready := false
	lastCursor := cursor
	// A selected timeout is a successful result only after a validated readiness
	// page. Earlier caller cancellation/deadlines always remain errors.
	interrupted := func() (MessageEventPage, error) {
		if err := ctx.Err(); err != nil {
			return MessageEventPage{}, contextError(err)
		}
		if err := outer.Err(); err != nil {
			return MessageEventPage{}, contextError(err)
		}
		if ready && errors.Is(context.Cause(stream), errMessageWaitExpired) {
			return MessageEventPage{Items: []MessageEvent{}, Cursor: lastCursor, TimedOut: true}, nil
		}
		return MessageEventPage{}, contextError(context.Cause(stream))
	}

	u := *c.base
	escaped := strings.TrimRight(u.EscapedPath(), "/")
	for _, segment := range c.projectPath("message-events", "stream") {
		escaped += "/" + url.PathEscape(segment)
	}
	u.Path, _ = url.PathUnescape(escaped)
	u.RawPath = escaped
	u.RawQuery = url.Values{"cursor": {cursor}, "limit": {strconv.Itoa(limit)}}.Encode()
	r, err := http.NewRequestWithContext(stream, http.MethodGet, u.String(), nil)
	if err != nil {
		return MessageEventPage{}, validation()
	}
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("X-FC-Instance", c.identity.InstanceID)
	r.Header.Set("X-FC-Worker", c.identity.WorkerID)
	r.Header.Set("X-FC-Session", c.sessionID)
	// The ordinary five-second whole-response timeout must not cut off an
	// established stream. This call owns both overrides; concurrent finite reads
	// retain their original client and transport settings.
	ownedTransport := c.transport.Clone()
	defer ownedTransport.CloseIdleConnections()
	ownedClient := *c.http
	ownedClient.Timeout = 0
	ownedClient.Transport = ownedTransport
	response, err := ownedClient.Do(r)
	if err != nil {
		if stream.Err() != nil {
			return interrupted()
		}
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return MessageEventPage{}, contextError(context.DeadlineExceeded)
		}
		return MessageEventPage{}, fail("connection_failed", "The service connection failed.", "Check the configured service address and transport.", 0)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return MessageEventPage{}, fail("redirect_rejected", "The service attempted an unsupported redirect.", "Configure the final service address; credentials are not forwarded through redirects.", response.StatusCode)
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || response.Header.Get("Content-Encoding") != "" {
		return MessageEventPage{}, invalidResponse()
	}
	if response.StatusCode != http.StatusOK {
		if media != "application/json" {
			return MessageEventPage{}, invalidResponse()
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, (64<<10)+1))
		if stream.Err() != nil {
			return interrupted()
		}
		if err != nil {
			return MessageEventPage{}, invalidResponse()
		}
		failure := c.messageWaitError(data, response.StatusCode)
		return MessageEventPage{}, failure
	}
	if media != "text/event-stream" {
		return MessageEventPage{}, invalidResponse()
	}
	reader := messageWaitReader{reader: bufio.NewReader(response.Body)}
	var previous int64
	for {
		frame, err := reader.next()
		if stream.Err() != nil {
			return interrupted()
		}
		if err != nil {
			return MessageEventPage{}, invalidResponse()
		}
		if frame.event == "error" {
			if frame.id != "" {
				return MessageEventPage{}, invalidResponse()
			}
			failure := c.messageWaitError([]byte(frame.data), 0)
			return MessageEventPage{}, failure
		}
		if frame.event != "message-events" {
			return MessageEventPage{}, invalidResponse()
		}
		var page MessageEventPage
		if decodeResponse([]byte(frame.data), &page) != nil || page.Items == nil || len(page.Items) > limit || !validOpaqueCursor(page.Cursor) || page.Cursor != frame.id || page.TimedOut || page.HasMore && len(page.Items) == 0 || c.hasSensitive(&page) {
			return MessageEventPage{}, invalidResponse()
		}
		sent := make([]MessageEvent, 0, len(page.Items))
		for _, item := range page.Items {
			if !c.validMessageEvent(item) || item.Sequence <= previous {
				return MessageEventPage{}, invalidResponse()
			}
			previous = item.Sequence
			if item.Type == "message.sent" {
				sent = append(sent, item)
			}
		}
		if stream.Err() != nil {
			return interrupted()
		}
		lastCursor = page.Cursor
		if !ready {
			readyTimer.Stop()
			ready = true
			waitTimer = time.AfterFunc(time.Duration(waitSeconds)*time.Second, func() { cancelStream(errMessageWaitExpired) })
		}
		if len(sent) > 0 {
			page.Items = sent
			return page, nil
		}
	}
}

func (c *Client) messageWaitError(data []byte, status int) *Error {
	failure, valid := c.serviceError(data, status)
	if valid && failure.Code == "resync_required" {
		if status == 0 {
			failure.Status = http.StatusConflict
		}
		failure.Action = "Read a fresh bound-worker inbox snapshot, then continue from its event cursor; pending messages remain unacknowledged."
	}
	return failure
}

type messageWaitFrame struct{ event, id, data string }

// Each byte, including ignored comments and framing, consumes the aggregate
// wire budget. Buffers are additionally bounded by the current frame budget.
type messageWaitReader struct {
	reader       *bufio.Reader
	total, frame int
	optionalLF   bool
}

func (r *messageWaitReader) byte() (byte, error) {
	b, err := r.reader.ReadByte()
	if err != nil {
		return 0, err
	}
	r.total++
	r.frame++
	if r.total > maxResponseBytes || r.frame > messageWaitFrameBytes {
		return 0, invalidResponse()
	}
	return b, nil
}

func (r *messageWaitReader) line() (string, error) {
	var line []byte
	for {
		b, err := r.byte()
		if err != nil {
			return "", err
		}
		if r.optionalLF {
			r.optionalLF = false
			if b == '\n' {
				continue
			}
		}
		if b == '\r' {
			r.optionalLF = true
			break
		}
		if b == '\n' {
			break
		}
		line = append(line, b)
	}
	if !utf8.Valid(line) {
		return "", invalidResponse()
	}
	return string(line), nil
}

func (r *messageWaitReader) next() (messageWaitFrame, error) {
	var frame messageWaitFrame
	var payload strings.Builder
	var event, id, data bool
	for {
		line, err := r.line()
		if err != nil {
			return messageWaitFrame{}, err
		}
		if line == "" {
			r.frame = 0
			if !event && !id && !data {
				continue
			}
			if !event || !data {
				return messageWaitFrame{}, invalidResponse()
			}
			frame.data = payload.String()
			return frame, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			if event || value == "" {
				return messageWaitFrame{}, invalidResponse()
			}
			event, frame.event = true, value
		case "id":
			if id || !validOpaqueCursor(value) {
				return messageWaitFrame{}, invalidResponse()
			}
			id, frame.id = true, value
		case "data":
			if data {
				payload.WriteByte('\n')
			}
			payload.WriteString(value)
			data = true
		default:
			return messageWaitFrame{}, invalidResponse()
		}
	}
}
