package delivery

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Outbound request headers. They form the documented contract with receivers.
const (
	HeaderEventID        = "X-RelayForge-Event-Id"
	HeaderDeliveryID     = "X-RelayForge-Delivery-Id"
	HeaderAttempt        = "X-RelayForge-Attempt"
	HeaderEventTimestamp = "X-RelayForge-Event-Timestamp"
	HeaderTimestamp      = "X-RelayForge-Timestamp"
	HeaderSignature      = "X-RelayForge-Signature"
	HeaderKeyVersion     = "X-RelayForge-Key-Version"

	signatureVersion = "v1"
	secretPrefix     = "rfd_"
	secretBytes      = 32
)

// GenerateDestinationSecret returns a new destination signing secret: "rfd_"
// followed by 256 bits from crypto/rand in unpadded base64url. The prefix
// differs from ingestion secrets ("rfs_") so the two can never be confused.
func GenerateDestinationSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate destination secret: %w", err)
	}
	return secretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// DestinationSecretAAD is the additional authenticated data that binds a
// destination secret ciphertext to its row. It differs from the ingestion
// endpoint's, so a ciphertext of one kind never opens as the other.
func DestinationSecretAAD(destinationID string) []byte {
	return []byte("relayforge:destination:v1:" + destinationID)
}

// canonical builds the signed message:
//
//	"v1." + timestamp + "." + deliveryID + "." + eventID + "." + payload
//
// The identifiers are included so that headers cannot be swapped between
// requests without invalidating the signature. The payload is the exact stored bytes.
func canonical(timestamp, deliveryID, eventID string, body []byte) []byte {
	msg := make([]byte, 0, len(timestamp)+len(deliveryID)+len(eventID)+len(body)+8)
	msg = append(msg, signatureVersion+"."...)
	msg = append(msg, timestamp...)
	msg = append(msg, '.')
	msg = append(msg, deliveryID...)
	msg = append(msg, '.')
	msg = append(msg, eventID...)
	msg = append(msg, '.')
	return append(msg, body...)
}

func mac(secret string, msg []byte) []byte {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(msg)
	return m.Sum(nil)
}

// SignOutbound returns the X-RelayForge-Signature header value:
// "v1=" followed by the lowercase hexadecimal HMAC-SHA256 of the canonical
// message, keyed with the destination secret string (its UTF-8 bytes).
func SignOutbound(secret, timestamp, deliveryID, eventID string, body []byte) string {
	return signatureVersion + "=" + hex.EncodeToString(mac(secret, canonical(timestamp, deliveryID, eventID, body)))
}

// VerifyOutbound is the receiver-side check, kept here so tests and documentation
// share one implementation. The comparison is constant time. A valid signature
// proves who signed the message and that it was not altered; it does not prevent
// replay by itself (see the timestamp guidance in the documentation).
func VerifyOutbound(secret, timestamp, deliveryID, eventID string, body []byte, signature string) bool {
	hexPart, ok := strings.CutPrefix(signature, signatureVersion+"=")
	if !ok || len(hexPart) != 2*sha256.Size || !isLowerHex(hexPart) {
		return false
	}
	got, err := hex.DecodeString(hexPart)
	if err != nil {
		return false
	}
	return hmac.Equal(got, mac(secret, canonical(timestamp, deliveryID, eventID, body)))
}

// OutboundParams are the inputs of one outgoing request.
type OutboundParams struct {
	Secret          string
	SecretVersion   int64
	DeliveryID      string
	EventID         string
	Attempt         int
	ContentType     string
	EventReceivedAt time.Time
	SignedAt        time.Time
	Body            []byte
}

// OutboundHeaders builds the complete, fixed header set of a delivery request.
// The delivery and event identifiers and the event timestamp are stable across
// attempts; the signature timestamp and the attempt number change per attempt.
func OutboundHeaders(p OutboundParams) map[string]string {
	ts := strconv.FormatInt(p.SignedAt.Unix(), 10)
	return map[string]string{
		"Content-Type":       p.ContentType,
		HeaderEventID:        p.EventID,
		HeaderDeliveryID:     p.DeliveryID,
		HeaderAttempt:        strconv.Itoa(p.Attempt),
		HeaderEventTimestamp: strconv.FormatInt(p.EventReceivedAt.Unix(), 10),
		HeaderTimestamp:      ts,
		HeaderKeyVersion:     strconv.FormatInt(p.SecretVersion, 10),
		HeaderSignature:      SignOutbound(p.Secret, ts, p.DeliveryID, p.EventID, p.Body),
	}
}

// isLowerHex accepts only lowercase hexadecimal digits, so there is exactly one
// accepted spelling of a signature.
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
