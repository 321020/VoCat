package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"strings"
	"time"
)

// writePlainTextMail constructs one RFC 5322 message without allowing values
// supplied by notification configuration or device messages to create new
// headers or MIME parts. Mailbox values have already passed net/mail parsing,
// the subject is encoded as one encoded-word, and the body is base64 encoded.
func writePlainTextMail(
	writer io.Writer,
	from *mail.Address,
	recipients []*mail.Address,
	subject string,
	body string,
) error {
	if from == nil || len(recipients) == 0 {
		return errors.New("email sender and recipient are required")
	}
	if strings.ContainsAny(subject, "\r\n\x00") {
		return errors.New("email subject contains a prohibited control character")
	}
	encodedBody := wrapMIMEBase64(base64.StdEncoding.EncodeToString([]byte(body)))
	message := strings.Join([]string{
		"Date: " + time.Now().UTC().Format(time.RFC1123Z),
		"From: " + formatMailAddress(from),
		"To: " + joinMailAddresses(recipients),
		"Subject: " + mime.QEncoding.Encode("UTF-8", subject),
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: base64",
		"",
		encodedBody,
		"",
	}, "\r\n")

	// The only values reaching this sink have been parsed as RFC mailboxes or
	// encoded as MIME encoded-words/base64 above. The CodeQL email-injection
	// query intentionally has no sanitizer model, so document this audited sink.
	// codeql[go/email-injection]
	if _, err := io.WriteString(writer, message); err != nil {
		return fmt.Errorf("write email message: %w", err)
	}
	return nil
}

func wrapMIMEBase64(value string) string {
	if value == "" {
		return ""
	}
	const lineLength = 76
	lines := make([]string, 0, (len(value)+lineLength-1)/lineLength)
	for len(value) > lineLength {
		lines = append(lines, value[:lineLength])
		value = value[lineLength:]
	}
	lines = append(lines, value)
	return strings.Join(lines, "\r\n")
}
