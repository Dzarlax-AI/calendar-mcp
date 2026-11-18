package cachedpages

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"calendar-mcp/internal/credentials"
)

const (
	maxCursorTimeBytes = 128
	maxCursorIDBytes   = 1024
)

var errInvalidCursor = errors.New("invalid page cursor")

// Codec encrypts keyset positions using the configured credential cipher and
// cursor-specific, binding-specific associated data.
type Codec struct {
	cipher *credentials.Cipher
	now    func() time.Time
}

func NewCodec(cipher *credentials.Cipher, now func() time.Time) *Codec {
	if now == nil {
		now = time.Now
	}
	return &Codec{cipher: cipher, now: now}
}

func (c *Codec) Encode(position Position, binding [32]byte, expiry time.Time) (string, error) {
	if c == nil || c.cipher == nil {
		return "", errors.New("page cursor cipher is unavailable")
	}
	if !expiry.After(c.now()) {
		return "", errors.New("page cursor expiry must be in the future")
	}
	if err := validatePosition(position); err != nil {
		return "", err
	}
	fields := [...]string{position.StartAt, position.StartDate, position.EventID, position.CalendarID}
	plain := make([]byte, 14, 14+8+len(position.StartAt)+len(position.StartDate)+len(position.EventID)+len(position.CalendarID))
	plain[0] = 1
	if position.AllDay {
		plain[1] = 1
	}
	if position.Partial {
		plain[1] |= 2
	}
	binary.BigEndian.PutUint64(plain[2:10], uint64(expiry.Unix()))
	binary.BigEndian.PutUint32(plain[10:14], uint32(expiry.Nanosecond()))
	for _, field := range fields {
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(field)))
		plain = append(plain, length[:]...)
		plain = append(plain, field...)
	}
	envelope, err := c.cipher.Encrypt(plain, cursorAAD(binding))
	if err != nil {
		return "", fmt.Errorf("encrypt page cursor: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(envelope)
	if len(token) > MaxTokenBytes {
		return "", errors.New("page cursor exceeds maximum token size")
	}
	return token, nil
}

func (c *Codec) Decode(token string, binding [32]byte) (Position, time.Time, error) {
	if c == nil || c.cipher == nil {
		return Position{}, time.Time{}, errors.New("page cursor cipher is unavailable")
	}
	if len(token) == 0 || len(token) > MaxTokenBytes || !isASCII(token) {
		return Position{}, time.Time{}, errInvalidCursor
	}
	envelope, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil {
		return Position{}, time.Time{}, errInvalidCursor
	}
	plain, err := c.cipher.Decrypt(envelope, cursorAAD(binding))
	if err != nil {
		return Position{}, time.Time{}, errInvalidCursor
	}
	position, expiry, err := decodeCursorPlaintext(plain)
	if err != nil || !expiry.After(c.now()) {
		return Position{}, time.Time{}, errInvalidCursor
	}
	return position, expiry, nil
}

func validatePosition(position Position) error {
	for i, field := range [...]string{position.StartAt, position.StartDate, position.EventID, position.CalendarID} {
		limit := maxCursorIDBytes
		if i < 2 {
			limit = maxCursorTimeBytes
		}
		if len(field) > limit || !utf8.ValidString(field) {
			return errors.New("page cursor position field is invalid")
		}
	}
	if position.EventID == "" || position.CalendarID == "" {
		return errors.New("page cursor event and calendar IDs are required")
	}
	if position.AllDay {
		if position.StartAt != "" || position.StartDate == "" {
			return errors.New("all-day cursor requires only a start date")
		}
		if _, err := time.Parse("2006-01-02", position.StartDate); err != nil {
			return errors.New("all-day cursor start date is invalid")
		}
	} else {
		if position.StartAt == "" || position.StartDate != "" {
			return errors.New("timed cursor requires only a start time")
		}
		// Timed keys retain their SQLite text representation verbatim so a
		// subsequent seek compares with the exact indexed value. The reader
		// owns validation and provider-specific normalization.
	}
	return nil
}

func decodeCursorPlaintext(plain []byte) (Position, time.Time, error) {
	if len(plain) < 22 || plain[0] != 1 || plain[1] > 3 {
		return Position{}, time.Time{}, errInvalidCursor
	}
	seconds := int64(binary.BigEndian.Uint64(plain[2:10]))
	nanos := binary.BigEndian.Uint32(plain[10:14])
	if nanos >= 1_000_000_000 {
		return Position{}, time.Time{}, errInvalidCursor
	}
	expiry := time.Unix(seconds, int64(nanos)).UTC()
	position := Position{AllDay: plain[1]&1 != 0, Partial: plain[1]&2 != 0}
	fields := []*string{&position.StartAt, &position.StartDate, &position.EventID, &position.CalendarID}
	offset := 14
	for i, field := range fields {
		if offset+2 > len(plain) {
			return Position{}, time.Time{}, errInvalidCursor
		}
		length := int(binary.BigEndian.Uint16(plain[offset : offset+2]))
		offset += 2
		limit := maxCursorIDBytes
		if i < 2 {
			limit = maxCursorTimeBytes
		}
		if length > limit || offset+length > len(plain) {
			return Position{}, time.Time{}, errInvalidCursor
		}
		value := plain[offset : offset+length]
		if !utf8.Valid(value) {
			return Position{}, time.Time{}, errInvalidCursor
		}
		*field = string(value)
		offset += length
	}
	if offset != len(plain) || validatePosition(position) != nil {
		return Position{}, time.Time{}, errInvalidCursor
	}
	return position, expiry, nil
}

func cursorAAD(binding [32]byte) []byte {
	const purpose = "calendar-cached-pages-cursor:v1\x00"
	data := make([]byte, 0, len(purpose)+len(binding))
	data = append(data, purpose...)
	return append(data, binding[:]...)
}

func isASCII(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] > 0x7f {
			return false
		}
	}
	return true
}
