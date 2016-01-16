// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/

package hawk

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"strconv"
	"strings"
	"text/scanner"
)

type Algorithm string

func (a *Algorithm) Scan(src interface{}) error {
	*a = "sha256"
	return nil
}

const (
	SHA256Algorithm  Algorithm = "sha256"
	DefaultAlgorithm           = SHA256Algorithm
)

type Authenticator struct {
	credentialsStore CredentialsStore
	replayChecker    ReplayChecker
}

func NewAuthenticator(credentialsStore CredentialsStore, replayChecker ReplayChecker) *Authenticator {
	return &Authenticator{
		credentialsStore: credentialsStore,
		replayChecker:    replayChecker,
	}
}

type Key struct {
	Identifier string
	Secret     []byte
	Algorithm  Algorithm
}

var MalformedParametersErr = errors.New("Malformed Parameters")
var MalformedCredentialsErr = errors.New("Malformed Credentials")

//
type Parameters struct {
	Id        string
	Timestamp int64
	Nonce     string
	Ext       string
	Mac       []byte
	Hash      []byte
}

func parseString(s string) string {
	return strings.Trim(s, `"`) // TODO: We really need to find out how strings in Hawk are encoded, maybe we need to deal with things like \t ?
}

func parseParameters(src string) (Parameters, error) {
	items := make(map[string]string)

	s := scanner.Scanner{Mode: scanner.ScanIdents | scanner.ScanChars | scanner.ScanStrings}
	s.Init(strings.NewReader(src))

	for {
		tok := s.Scan()
		if tok != scanner.Ident {
			return Parameters{}, MalformedParametersErr
		}
		name := s.TokenText()

		tok = s.Scan()
		if tok != '=' {
			return Parameters{}, MalformedParametersErr
		}

		tok = s.Scan()
		if tok != scanner.String {
			return Parameters{}, MalformedParametersErr
		}
		value := s.TokenText()

		items[name] = parseString(value)

		tok = s.Scan()
		if tok == scanner.EOF {
			break
		}
		if tok != ',' {
			return Parameters{}, MalformedParametersErr
		}
	}

	// Now parse the items and setup a Parameters struct

	timestamp, err := strconv.ParseInt(items["ts"], 10, 64)
	if err != nil {
		return Parameters{}, err
	}

	decodedHash, err := base64.StdEncoding.DecodeString(items["hash"])
	if err != nil {
		return Parameters{}, err
	}

	decodedMac, err := base64.StdEncoding.DecodeString(items["mac"])
	if err != nil {
		return Parameters{}, err
	}

	return Parameters{
		Id:        items["id"],
		Timestamp: timestamp,
		Nonce:     items["nonce"],
		Ext:       items["ext"],
		Hash:      decodedHash,
		Mac:       decodedMac,
	}, nil
}

func validateParameters(parameters Parameters) error {
	if parameters.Id == "" {
		return MalformedParametersErr
	}
	if parameters.Timestamp == 0 {
		return MalformedParametersErr
	}
	if parameters.Nonce == "" {
		return MalformedParametersErr
	}
	if len(parameters.Mac) == 0 {
		return MalformedParametersErr
	}
	return nil
}

// Make sure the Credentials return a valid key
func validateCredentials(credentials Credentials) error {
	if credentials.Key().Identifier == "" || len(credentials.Key().Identifier) == 0 {
		return MalformedCredentialsErr
	}
	if credentials.Key().Algorithm != DefaultAlgorithm {
		return MalformedCredentialsErr
	}
	return nil
}

func GetRequestPath(r *http.Request) string {
	path := r.URL.Path
	if len(r.URL.RawQuery) != 0 {
		path = path + "?" + r.URL.RawQuery
	}
	if len(r.URL.Fragment) != 0 {
		path = path + "#" + r.URL.Fragment
	}
	return path
}

// TODO: Make sure the following two do the right thing when behind a proxy

func GetRequestHost(r *http.Request) string {
	hostPort := strings.Split(r.Host, ":")
	return hostPort[0]
}

func GetRequestPort(r *http.Request) int {
	host := r.Host
	if len(r.Header["X-Forwarded-Host"]) != 0 {
		host = r.Header["X-Forwarded-Host"][0]
	}
	hostPort := strings.Split(host, ":")
	if len(hostPort) == 2 {
		port, _ := strconv.Atoi(hostPort[1])
		return port
	} else {
		switch r.URL.Scheme {
		case "http":
			return 80
		case "https":
			return 443
		}
	}
	return 0
}

func GetRequestContentType(r *http.Request) string {
	return r.Header.Get("Content-Type")
}

func CalculatePayloadHash(r *http.Request) ([]byte, error) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}

	r.Body = NewClosingBytesReader(body)

	hash := sha256.New()
	hash.Write([]byte("hawk.1.payload\n"))
	hash.Write([]byte(GetRequestContentType(r)))
	hash.Write([]byte("\n"))
	hash.Write(body)
	hash.Write([]byte("\n"))
	return hash.Sum(nil), nil
}

func CalculateRequestSignature(r *http.Request, parameters Parameters, credentials Credentials) ([]byte, error) {
	var encodedPayloadHash string
	if len(parameters.Hash) != 0 {
		payloadHash, err := CalculatePayloadHash(r)
		if err != nil {
			return nil, err
		}
		encodedPayloadHash = base64.StdEncoding.EncodeToString(payloadHash)
	}

	parts := []string{
		"hawk.1.header",
		strconv.FormatInt(parameters.Timestamp, 10),
		parameters.Nonce,
		r.Method,
		GetRequestPath(r),
		GetRequestHost(r),
		strconv.Itoa(GetRequestPort(r)),
		encodedPayloadHash,
		parameters.Ext,
	}

	requestHeader := strings.Join(parts, "\n") + "\n"

	mac := hmac.New(sha256.New, credentials.Key().Secret)
	mac.Write([]byte(requestHeader))
	return mac.Sum(nil), nil
}

// CreateRequestHeader makes a Hawk authentication string that can be
// added to an http.Request
func CreateRequestHeader(r *http.Request, c Credentials, id string, timestamp int64, nonce string, ext string) (string, error) {

	p := Parameters{
		Id:        id,
		Timestamp: timestamp,
		Nonce:     nonce,
		Ext:       ext,
	}

	var mac, hash string

	if r.Body != nil {
		hashB, err := CalculatePayloadHash(r)
		if err != nil {
			return "", err
		}
		p.Hash = hashB
		hash = base64.StdEncoding.EncodeToString(hashB)
	}

	macB, err := CalculateRequestSignature(r, p, c)
	if err != nil {
		return "", err
	}

	mac = base64.StdEncoding.EncodeToString(macB)

	header := fmt.Sprintf(`Hawk id="%s", ts="%d", nonce="%s", ext="%s", mac="%s"`,
		id,
		timestamp,
		nonce,
		ext,
		mac,
	)

	if hash != "" {
		header = fmt.Sprintf(`%s, hash="%s"`, header, hash)
	}

	return header, nil
}

func (a *Authenticator) Authenticate(w http.ResponseWriter, r *http.Request) (Credentials, bool) {
	// Grab the Authorization Header

	authorization := r.Header.Get("Authorization")
	if len(authorization) == 0 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}

	tokens := strings.SplitN(authorization, " ", 2)
	if len(tokens) != 2 {
		http.Error(w, "Unsupported authorization method", http.StatusUnauthorized)
		return nil, false
	}
	if tokens[0] != "Hawk" {
		http.Error(w, "Unsupported authorization method", http.StatusUnauthorized)
		return nil, false
	}

	// Parse and validate the Hawk parameters

	parameters, err := parseParameters(tokens[1])
	if err != nil {
		http.Error(w, "Unable to parse Hawk parameters", http.StatusUnauthorized)
		return nil, false
	}

	if err = validateParameters(parameters); err != nil {
		http.Error(w, "Invalid Hawk parameters: "+err.Error(), http.StatusUnauthorized)
		return nil, false
	}

	// TODO: Check if this request has expired

	// Check if we have seen this request before

	requestId := fmt.Sprintf("%s:%d:%s", parameters.Timestamp, parameters.Id, parameters.Nonce)

	seenBefore, err := a.replayChecker.Check(requestId)
	if err != nil {
		// TODO: Unable to check means server error. Is there a better strategy?
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return nil, false
	}

	if seenBefore {
		http.Error(w, "Request has been seen before", http.StatusUnauthorized)
		return nil, false
	}

	if err := a.replayChecker.Remember(requestId); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return nil, false
	}

	// Find the user and keys

	credentials, err := a.credentialsStore.CredentialsForKeyIdentifier(parameters.Id)
	if err != nil {
		http.Error(w, "Internal server error:"+err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	if credentials == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}

	if err := validateCredentials(credentials); err != nil {
		http.Error(w, "Invalid credentials: "+err.Error(), http.StatusUnauthorized)
		return nil, false
	}

	// Check the Hawk request signature

	mac, err := CalculateRequestSignature(r, parameters, credentials)
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return nil, false
	}

	if !bytes.Equal(mac, parameters.Mac) {
		http.Error(w, "Signature Mismatch", http.StatusUnauthorized)
		return nil, false
	}

	// Return the credentials and parsed artifacts

	return credentials, true
}
