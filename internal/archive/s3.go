package archive

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// S3Backend stores objects in one bucket of an S3-compatible store, written
// for and tested against Garage. It uses the standard library, with SigV4
// signing here, since it needs only five calls.
//
// Put is create-only by HEAD then PUT, because Garage v2.5.0 ignores
// If-None-Match on PUT: a second PUT replaces the object (the contract test
// records this). That leaves a window between the two calls, which is safe
// for segments because each host writes only under v1/<host>/, its own
// writers hold the lock in Archiver.lock, and hosts/<host>.json refuses a
// second machine using the same name. Put still sends If-None-Match and
// honours a 412, so it stays correct on a store that implements it.
type S3Backend struct {
	Bucket, Region, KeyID, Secret string

	scheme, host string
	client       *http.Client
	pageSize     int // ListObjectsV2 max-keys; 0 leaves it to the server
	now          func() time.Time
	sleep        func(time.Duration)

	mu    sync.Mutex
	sizes map[string]int64 // from the last List, for status
}

const (
	s3Attempts       = 3
	s3RequestTimeout = 2 * time.Minute
	s3DialTimeout    = 10 * time.Second
)

// NewS3Backend takes the bucket's URL, https://HOST[:PORT]/BUCKET. Plain http
// is refused except on loopback: objects and credentials would cross the
// network readable.
func NewS3Backend(rawURL, keyID, secret, region string) (*S3Backend, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("archive: bad S3 URL %q (want https://HOST/BUCKET)", rawURL)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		return nil, fmt.Errorf("archive: S3 URL %q must be https (plain http only on loopback)", rawURL)
	}
	if u.User != nil {
		return nil, errors.New("archive: the S3 URL must not hold credentials; use the credentials file")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("archive: bad S3 URL %q (want https://HOST/BUCKET)", rawURL)
	}
	bucket := strings.Trim(u.Path, "/")
	if bucket == "" || strings.Contains(bucket, "/") {
		return nil, fmt.Errorf("archive: S3 URL %q must name one bucket: https://HOST/BUCKET", rawURL)
	}
	if keyID == "" || secret == "" {
		return nil, errors.New("archive: S3 credentials are incomplete")
	}
	if region == "" {
		region = "garage"
	}
	return &S3Backend{
		Bucket: bucket, Region: region, KeyID: keyID, Secret: secret,
		scheme: u.Scheme, host: u.Host,
		client: &http.Client{
			Timeout:   s3RequestTimeout,
			Transport: s3Transport(),
			// A redirect would resend a signed request somewhere it was not signed for.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		now: time.Now, sleep: time.Sleep,
	}, nil
}

func s3Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: s3DialTimeout}).DialContext
	return t
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadS3Credentials reads key_id=... and secret_key=... lines. The file must
// be private: its key can read and delete the whole bucket.
func LoadS3Credentials(path string) (keyID, secret string, err error) {
	path = expand(path)
	fi, err := os.Stat(path)
	if err != nil {
		return "", "", fmt.Errorf("archive: S3 credentials: %w", err)
	}
	if m := fi.Mode().Perm(); m&0o077 != 0 {
		return "", "", fmt.Errorf("archive: S3 credentials file %s is mode %04o: group or others can read it (chmod 600)", path, m)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("archive: S3 credentials: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return "", "", fmt.Errorf("archive: S3 credentials file %s: a line is not key=value", path)
		}
		switch strings.TrimSpace(k) {
		case "key_id":
			keyID = strings.TrimSpace(v)
		case "secret_key":
			secret = strings.TrimSpace(v)
		}
	}
	if keyID == "" || secret == "" {
		return "", "", fmt.Errorf("archive: S3 credentials file %s needs key_id= and secret_key=", path)
	}
	return keyID, secret, nil
}

// s3Error is a response the store refused. It never holds credentials.
type s3Error struct {
	Op, Key string
	Status  int
	Code    string
	Message string
}

func (e *s3Error) Error() string {
	msg := fmt.Sprintf("archive: S3 %s %s: HTTP %d", e.Op, e.Key, e.Status)
	if e.Code != "" {
		msg += " " + e.Code
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

type s3Response struct {
	status int
	header http.Header
	body   []byte
}

func s3Encode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

func hexSHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// sign returns the Authorization value for a request, per AWS Signature V4.
// headers holds every signed header, with lower-case names.
func (b *S3Backend) sign(method, path, query string, headers map[string]string, payloadHash string, at time.Time) string {
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, k := range names {
		canon.WriteString(k + ":" + strings.TrimSpace(headers[k]) + "\n")
	}
	signed := strings.Join(names, ";")
	request := strings.Join([]string{method, path, query, canon.String(), signed, payloadHash}, "\n")
	date := at.Format("20060102")
	scope := date + "/" + b.Region + "/s3/aws4_request"
	toSign := strings.Join([]string{"AWS4-HMAC-SHA256", at.Format("20060102T150405Z"), scope, hexSHA256([]byte(request))}, "\n")
	k := hmacSHA256([]byte("AWS4"+b.Secret), date)
	k = hmacSHA256(k, b.Region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		b.KeyID, scope, signed, hex.EncodeToString(hmacSHA256(k, toSign)))
}

// do sends one request, retrying what may be a blip: a transport failure, a
// 5xx or a 429. Everything this backend sends is safe to repeat.
func (b *S3Backend) do(method, key string, query url.Values, body []byte, extra map[string]string) (*s3Response, error) {
	var last error
	for attempt := 0; attempt < s3Attempts; attempt++ {
		if attempt > 0 {
			b.sleep(time.Duration(attempt) * time.Second)
		}
		r, err := b.once(method, key, query, body, extra)
		if err != nil {
			last = err
			continue
		}
		if r.status >= 500 || r.status == http.StatusTooManyRequests {
			last = b.refusal(method, key, r)
			continue
		}
		return r, nil
	}
	return nil, last
}

func (b *S3Backend) once(method, key string, query url.Values, body []byte, extra map[string]string) (*s3Response, error) {
	path := "/" + s3Encode(b.Bucket, false)
	if key != "" {
		path += "/" + s3Encode(key, true)
	}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, s3Encode(k, false)+"="+s3Encode(query.Get(k), false))
	}
	rawQuery := strings.Join(pairs, "&")

	at := b.now().UTC()
	payload := hexSHA256(body)
	headers := map[string]string{
		"host":                 b.host,
		"x-amz-content-sha256": payload,
		"x-amz-date":           at.Format("20060102T150405Z"),
	}
	for k, v := range extra {
		headers[k] = v
	}
	target := b.scheme + "://" + b.host + path
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("archive: S3 %s %s: %w", method, key, err)
	}
	req.ContentLength = int64(len(body))
	for k, v := range headers {
		if k != "host" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Authorization", b.sign(method, path, rawQuery, headers, payload, at))
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("archive: S3 %s %s: %w", method, key, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("archive: S3 %s %s: %w", method, key, err)
	}
	return &s3Response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

func (b *S3Backend) refusal(op, key string, r *s3Response) error {
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(r.body, &x)
	return &s3Error{Op: op, Key: key, Status: r.status, Code: x.Code, Message: x.Message}
}

func hasStatus(r *s3Response, codes ...int) bool {
	for _, c := range codes {
		if r.status == c {
			return true
		}
	}
	return false
}

// etagOf returns the object's MD5 from its ETag, or "" if the ETag is not an
// MD5 (an object uploaded in parts, for example by `aws s3 sync`).
func etagOf(r *s3Response) string {
	e := strings.Trim(r.header.Get("ETag"), `"`)
	if len(e) != 32 || strings.Contains(e, "-") {
		return ""
	}
	return strings.ToLower(e)
}

// Put stores data unless the key exists. See the type's comment.
func (b *S3Backend) Put(key string, data []byte) error {
	if err := validKey(key); err != nil {
		return err
	}
	head, err := b.do(http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return err
	}
	switch {
	case head.status == http.StatusOK:
		return ErrExists
	case head.status != http.StatusNotFound:
		return b.refusal("HEAD", key, head)
	}
	sum := md5.Sum(data)
	r, err := b.do(http.MethodPut, key, nil, data, map[string]string{
		"content-md5":   base64.StdEncoding.EncodeToString(sum[:]),
		"if-none-match": "*",
	})
	if err != nil {
		return err
	}
	if r.status == http.StatusPreconditionFailed {
		return ErrExists
	}
	if !hasStatus(r, http.StatusOK, http.StatusCreated, http.StatusNoContent) {
		return b.refusal("PUT", key, r)
	}
	if e := etagOf(r); e != "" && e != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("archive: S3 PUT %s: the store's MD5 %s does not match what was sent", key, e)
	}
	return nil
}

// Get returns the object, checking its MD5 when the ETag holds one.
func (b *S3Backend) Get(key string) ([]byte, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	r, err := b.do(http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if r.status == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if r.status != http.StatusOK {
		return nil, b.refusal("GET", key, r)
	}
	if e := etagOf(r); e != "" {
		sum := md5.Sum(r.body)
		if e != hex.EncodeToString(sum[:]) {
			return nil, fmt.Errorf("archive: S3 GET %s: the body does not match its MD5", key)
		}
	}
	return r.body, nil
}

// List returns every key that starts with prefix, sorted.
func (b *S3Backend) List(prefix string) ([]string, error) {
	var keys []string
	sizes := map[string]int64{}
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if b.pageSize > 0 {
			q.Set("max-keys", fmt.Sprint(b.pageSize))
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		r, err := b.do(http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		if r.status != http.StatusOK {
			return nil, b.refusal("LIST", prefix, r)
		}
		var page struct {
			Contents []struct {
				Key  string `xml:"Key"`
				Size int64  `xml:"Size"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(r.body, &page); err != nil {
			return nil, fmt.Errorf("archive: S3 LIST %s: %w", prefix, err)
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
			sizes[c.Key] = c.Size
		}
		if !page.IsTruncated {
			break
		}
		// A short listing would read as sessions having vanished.
		if page.NextContinuationToken == "" || page.NextContinuationToken == token {
			return nil, fmt.Errorf("archive: S3 LIST %s: the listing is truncated but has no way to continue", prefix)
		}
		token = page.NextContinuationToken
	}
	sort.Strings(keys)
	b.mu.Lock()
	b.sizes = sizes
	b.mu.Unlock()
	return keys, nil
}

// Delete removes an object. A key that is not there is not an error.
func (b *S3Backend) Delete(key string) error {
	if err := validKey(key); err != nil {
		return err
	}
	r, err := b.do(http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	if !hasStatus(r, http.StatusOK, http.StatusNoContent, http.StatusNotFound) {
		return b.refusal("DELETE", key, r)
	}
	return nil
}

// Size is the stored size of an object (for status), from the last List when
// it saw the key, so status does not make one request per segment.
func (b *S3Backend) Size(key string) (int64, error) {
	b.mu.Lock()
	n, ok := b.sizes[key]
	b.mu.Unlock()
	if ok {
		return n, nil
	}
	r, err := b.do(http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return 0, err
	}
	if r.status == http.StatusNotFound {
		return 0, fmt.Errorf("%w: %s", ErrNotExist, key)
	}
	if r.status != http.StatusOK {
		return 0, b.refusal("HEAD", key, r)
	}
	var size int64
	if _, err := fmt.Sscan(r.header.Get("Content-Length"), &size); err != nil {
		return 0, fmt.Errorf("archive: S3 HEAD %s: no usable Content-Length", key)
	}
	return size, nil
}
