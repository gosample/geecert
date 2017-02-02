/*

Copyright 2017 Continusec Pty Ltd

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

*/

package geecert

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/hydrogen18/stoppableListener"
	homedir "github.com/mitchellh/go-homedir"
	"github.com/pkg/browser"

	pb "github.com/continusec/geecert/sso"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	context "golang.org/x/net/context"

	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
)

const (
	AuthURI  = "https://accounts.google.com/o/oauth2/auth"
	TokenURI = "https://accounts.google.com/o/oauth2/token"
	CertURL  = "https://www.googleapis.com/oauth2/v1/certs"

	RedirectOOB       = "urn:ietf:wg:oauth:2.0:oob"
	RedirectLocalhost = "http://localhost"
)

type ClientAppConfiguration struct {
	HostedDomain       string // Matches against field in Google response. Should be your domain name.
	ClientID           string // Client ID as configured with Google: https://console.developers.google.com/
	ClientNotSoSecret  string // Client "Secret" corresponding to the Client ID. Note, despite the name, this is not really a secret nor intended to be.
	GRPCPEMCertificate string // If set, Self-signed GRPC server certificate, else GRPCPEMCertificatePath is used
	GRPCServer         string // server:host
	CredentialFileName string // e.g. .geecerttoken

	GRPCPEMCertificatePath string // If set, path to PEM for server certificate

	OverrideMachinePolicy bool // If true, override machine policy such as requiring FDE
	OverrideGrpcSecurity  bool // If true, allow insecure connection to gRPC server
	UseSystemCaForCert    bool // If true, use a system CA instead of self-signed certificate

	ShortlivedKeyName string // e.g. id_orgname_shortlived_rsa
	SectionIdentifier string // e.g. ORGNAME-CA
}

var (
	ErrUserDenied       = errors.New("User clicked deny.")
	ErrWrongKeyFileType = errors.New("Wrong key file type.")
	ErrWrongCertType    = errors.New("Wrong cert file type.")
)

// Try to launch a browser, redirect to local server etc etc
// Return code, redirect URI, error
func DoBrowserDance(config *ClientAppConfiguration) (string, string, error) {
	// Find a free port number
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return "", "", err
	}

	// Bind a listener
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return "", "", err
	}

	// Make it stoppable
	stoppable, err := stoppableListener.New(listener)
	if err != nil {
		return "", "", err
	}

	// Get the post out
	port := listener.Addr().(*net.TCPAddr).Port

	// Construct the redirect URL
	redir := RedirectLocalhost + ":" + strconv.Itoa(port)

	// Send the user there
	urlToVisit := AuthURI + "?" + url.Values{
		"scope":         {"email"},
		"redirect_uri":  {redir},
		"response_type": {"code"},
		"client_id":     {config.ClientID},
	}.Encode()

	err = browser.OpenURL(urlToVisit)
	if err != nil {
		return "", "", err
	}

	fmt.Println(`Please click the "Allow" button in your browser to authorize our SSO tool.`)

	// Wait for the server to get the code
	var code string
	err = http.Serve(stoppable, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := r.FormValue("code")
		switch {
		case len(c) > 0:
			w.Write([]byte("Authorization code received. Please close this window and return to your terminal to complete the process."))
			code = c
			stoppable.Stop()
		case r.FormValue("error") == "access_denied":
			w.Write([]byte("We'll miss you. Please close this window and return to your terminal."))
			stoppable.Stop()
		default:
			w.Write([]byte("Error - please try again."))
		}
	}))
	switch err {
	case nil:
		// pass
	case stoppableListener.StoppedError:
		// pass
	default:
		return "", "", err
	}

	if len(code) < 1 {
		return "", "", ErrUserDenied
	}

	log.Print("Authorization code received.")

	return code, redir, nil
}

func DoOOBDance(config *ClientAppConfiguration) (string, string, error) {
	// Send the user there
	urlToVisit := AuthURI + "?" + url.Values{
		"scope":         {"email"},
		"redirect_uri":  {RedirectOOB},
		"response_type": {"code"},
		"client_id":     {config.ClientID},
	}.Encode()

	fmt.Printf("Please visit (in your browser):\n%s\n\nAnd then paste the code received here: ", urlToVisit)

	// If we don't have one, then prompt for it
	var code string
	for len(code) < 1 {
		_, err := fmt.Scanln(&code)
		if err != nil {
			return "", "", err
		}
	}

	return code, RedirectOOB, nil
}

func SwapCodeForTokens(config *ClientAppConfiguration, code, redir string) (*CachedCreds, error) {
	log.Print("Exchanging authorization code for long-lived credentials.")

	// Now we have an authorization code, exchange this for the good stuff
	resp, err := http.PostForm(TokenURI, url.Values{
		"code":          {code},
		"client_id":     {config.ClientID},
		"client_secret": {config.ClientNotSoSecret},
		"redirect_uri":  {redir},
		"grant_type":    {"authorization_code"},
	})
	if err != nil {
		return nil, err
	}

	// Always read body, even if not 200 as it can contain info about the err
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Fail if not OK
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Unexpected server response: " + resp.Status + " " + string(body))
	}

	var creds CachedCreds
	err = json.Unmarshal(body, &creds)
	if err != nil {
		return nil, err
	}

	log.Print("Received long-lived credentials.")

	return &creds, nil
}

func SwapRefreshForTokens(config *ClientAppConfiguration, refreshToken string) (*CachedCreds, error) {
	log.Print("Sending refresh token for short-lived credentials.")

	// Now we have an authorization code, exchange this for the good stuff
	resp, err := http.PostForm(TokenURI, url.Values{
		"refresh_token": {refreshToken},
		"client_id":     {config.ClientID},
		"client_secret": {config.ClientNotSoSecret},
		"grant_type":    {"refresh_token"},
	})
	if err != nil {
		return nil, err
	}

	// Always read body, even if not 200 as it can contain info about the err
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Fail if not OK
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Unexpected server response: " + resp.Status + " " + string(body))
	}

	var creds CachedCreds
	err = json.Unmarshal(body, &creds)
	if err != nil {
		return nil, err
	}

	// Refresh token is not return to us
	creds.RefreshToken = refreshToken

	log.Print("Received new short-lived credentials.")

	return &creds, nil
}

type CachedCreds struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
}

// Prompt user to
func Reauthorize(config *ClientAppConfiguration, path string) error {
	// First try the browser dance as it's easier for the user
	code, redir, err := DoBrowserDance(config)
	switch err {
	case nil:
		// yay, pass!
	case ErrUserDenied:
		return err
	default:
		// Fall back to OOB dance
		code, redir, err = DoOOBDance(config)
	}
	if err != nil {
		return err
	}

	// Swap authorization code for tokens
	creds, err := SwapCodeForTokens(config, code, redir)
	if err != nil {
		return err
	}

	// Save creds off.
	err = SaveCreds(path, creds)
	if err != nil {
		return err
	}

	// All good
	return nil
}

func LoadCreds(path string) (*CachedCreds, error) {
	body, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var creds CachedCreds
	err = json.Unmarshal(body, &creds)
	if err != nil {
		return nil, err
	}

	return &creds, nil
}

func SaveCreds(path string, creds *CachedCreds) error {
	body, err := json.Marshal(creds)
	if err != nil {
		return err
	}

	err = ioutil.WriteFile(path, body, 0600)
	if err != nil {
		return err
	}

	log.Print("Saved credentials to ", path)
	return nil
}

// sshDir is the absolute path
// homePathToSSHDir is the path to use inside of a config file, this should contain a ~
// rather than be absolute as it allows this .ssh dir to be mounted as a volume inside of Docker
// and work well.
func FetchCerts(config *ClientAppConfiguration, idToken string, sshDir string, homePathToSSHDir string) error {
	log.Println("Generating new private key.")
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}

	ourPubKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return err
	}
	ourPubKeyString := base64.StdEncoding.EncodeToString(ourPubKey.Marshal())

	// Get certs
	var dialOptions []grpc.DialOption
	if config.OverrideGrpcSecurity {
		// use system CA pool but disable cert validation
		log.Println("WARNING: Disabling TLS authentication when connecting to SSO gRPC server")
		dialOptions = append(dialOptions, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	} else if len(config.GRPCPEMCertificatePath) > 0 {
		tc, err := credentials.NewClientTLSFromFile(config.GRPCPEMCertificatePath, "")
		if err != nil {
			return err
		}
		dialOptions = append(dialOptions, grpc.WithTransportCredentials(tc))
	} else if config.UseSystemCaForCert {
		dialOptions = append(dialOptions, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{}))) // uses the system CA pool
	} else {
		// use baked in cert
		cp := x509.NewCertPool()
		if !cp.AppendCertsFromPEM([]byte(config.GRPCPEMCertificate)) {
			return errors.New("Unable to understand baked-in cert.")
		}
		dialOptions = append(dialOptions, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{RootCAs: cp})))
	}

	conn, err := grpc.Dial(config.GRPCServer, dialOptions...)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewGeeCertServerClient(conn)

	log.Println("Requesting fresh certificates...")
	resp, err := client.GetSSHCerts(context.Background(), &pb.SSHCertsRequest{
		IdToken:   idToken,
		PublicKey: ourPubKeyString,
	})
	if err != nil {
		return err
	}

	if resp.Status != 0 {
		return errors.New(fmt.Sprintf("Bad response form server: %#v", resp))
	}

	log.Println("Received new certificates from server.")

	// Create ssh dir if not exists
	_, err = os.Stat(sshDir)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("Creating SSH config directory.")
			err = os.Mkdir(sshDir, 0700)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	log.Println("Writing new private key.")
	err = SafeSave(filepath.Join(sshDir, config.ShortlivedKeyName), pem.EncodeToMemory(
		&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		},
	), 0600)
	if err != nil {
		return err
	}

	// And public key too, not that it should be needed in theory, but SSH moans if it isn't there.
	// Works in openssh 6.9. Broken in 7.2. Patch has been submitted to openssh team.
	err = SafeSave(filepath.Join(sshDir, config.ShortlivedKeyName+".pub"), []byte("ssh-rsa "+ourPubKeyString+" ignorethiscomment\n"), 0644)
	if err != nil {
		return err
	}

	log.Println("Installing new certificate. For more info, run: ssh-keygen -Lf ~/.ssh/" + config.ShortlivedKeyName + "-cert.pub")
	err = SafeSave(filepath.Join(sshDir, config.ShortlivedKeyName+"-cert.pub"), []byte(resp.Certificate), 0644)
	if err != nil {
		return err
	}

	// Update known hosts
	err = ReplaceSectionOfFile(config.SectionIdentifier, filepath.Join(sshDir, "known_hosts"), resp.CertificateAuthorities, 0644, "Updating known_hosts certificate authorities.")
	if err != nil {
		return err
	}

	// Update SSH config
	cnf := make([]string, len(resp.Config))
	for i, line := range resp.Config {
		cnf[i] = strings.Replace(line, "$CERTNAME", filepath.Join(homePathToSSHDir, config.ShortlivedKeyName), -1)
	}
	err = ReplaceSectionOfFile(config.SectionIdentifier, filepath.Join(sshDir, "config"), cnf, 0644, "Updating ssh config file to use certificates.")
	if err != nil {
		return err
	}

	// Check if ssh-agent is running, and if so, add our cert
	authSock := os.Getenv("SSH_AUTH_SOCK")
	if len(authSock) != 0 {
		log.Println("SSH_AUTH_SOCK detected, adding certificate to ssh-agent.")
		// Try to add our cert
		pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(resp.Certificate))
		if err != nil {
			return err
		}
		cert, ok := pk.(*ssh.Certificate)
		if !ok {
			return ErrWrongCertType
		}
		ttl := int64(cert.ValidBefore) - time.Now().Unix()
		log.Printf("Certificate will be added with TTL of %d seconds.\n", ttl)

		agentSocket, err := net.Dial("unix", authSock)
		if err != nil {
			return err
		}
		sshAgent := agent.NewClient(agentSocket)
		err = sshAgent.Add(agent.AddedKey{
			PrivateKey:   privateKey,
			Certificate:  cert,
			LifetimeSecs: uint32(ttl),
		})
		if err != nil {
			return err
		}
	}

	return nil
}

/* Deletes section with name:

# AUTOGENERATED:BEGIN:name
...
# AUTOGENERATED:END:name

and adds new section at end with same.
*/
func ReplaceSectionOfFile(name string, path string, lines []string, perm os.FileMode, messageIfChanged string) error {
	startMarker := "# AUTOGENERATED:BEGIN:" + name
	endMarker := "# AUTOGENERATED:END:" + name

	// Read contents of old file
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) { // it's OK if it doesn't exist
			contents = nil
		} else {
			return err
		}
	}

	// Copy contents to buffer, skipping over our section
	var output []string
	include := true
	for _, line := range strings.Split(string(contents), "\n") {
		if strings.HasPrefix(line, startMarker) {
			include = false
		} else if strings.HasPrefix(line, endMarker) {
			include = true
		} else {
			if include {
				output = append(output, line)
			}
		}
	}

	// Remove any trailing empty lines
	for len(output) > 0 && len(output[len(output)-1]) == 0 {
		output = output[:len(output)-1]
	}

	// If we have a section to append, append it
	if len(lines) > 0 {
		output = append(output, "")
		output = append(output, startMarker+" - DO NOT EDIT BETWEEN MARKERS!")
		output = append(output, lines...)
		output = append(output, endMarker+" - DO NOT EDIT BETWEEN MARKERS!")
	}

	// Always finish with a new line
	output = append(output, "")

	// Only log and write if we've changed.
	newContents := []byte(strings.Join(output, "\n"))
	if !bytes.Equal(contents, newContents) {
		// Save it out
		log.Println(messageIfChanged)
		err = SafeSave(path, newContents, perm)
		if err != nil {
			return err
		}
	}

	return nil
}

func SafeSave(path string, contents []byte, perm os.FileMode) error {
	pathToNew := path + ".tmpfornew"
	err := ioutil.WriteFile(pathToNew, contents, perm)
	if err != nil {
		return err
	}
	err = os.Rename(pathToNew, path)
	if err != nil {
		return err
	}
	return nil
}

// We can use this to soft-enforce only giving certificates out if reasonable precautions
// are in place in the client device, e.g. enforce full disk encryption with machine passcode.
func ValidateMachineIsSuitable(config *ClientAppConfiguration) error {
	if config.OverrideMachinePolicy {
		log.Println("WARNING: Overriding machine policy.")
		return nil
	}

	switch runtime.GOOS {
	case "darwin":
		// on Mac, require full disk encryption be enabled
		out, err := exec.Command("fdesetup", "status").Output()
		if err != nil {
			return err
		}

		if strings.Index(string(out), "FileVault is On") < 0 {
			log.Fatal("FileVault must be enabled if you want SSH certificates. Please enable and then retry (or, re-run with --override_machine_policy)")
		}

		return nil
	default:
		// for now, allow
		return nil
	}
}

func loadSigningKey(config *ClientAppConfiguration) (ssh.Signer, *ssh.Certificate, error) {
	hd, err := homedir.Dir()
	if err != nil {
		return nil, nil, err
	}

	sshDir := filepath.Join(hd, ".ssh")

	data, err := ioutil.ReadFile(filepath.Join(sshDir, config.ShortlivedKeyName))
	if err != nil {
		return nil, nil, err
	}

	sshPublicKey, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, nil, err
	}

	certData, err := ioutil.ReadFile(filepath.Join(sshDir, config.ShortlivedKeyName+"-cert.pub"))
	if err != nil {
		return nil, nil, err
	}

	sshCert, _, _, _, err := ssh.ParseAuthorizedKey(certData)
	if err != nil {
		return nil, nil, err
	}
	actCert, ok := sshCert.(*ssh.Certificate)
	if !ok {
		return nil, nil, ErrWrongKeyFileType
	}

	cs, err := ssh.NewCertSigner(actCert, sshPublicKey)
	if err != nil {
		return nil, nil, err
	}

	return cs, actCert, nil
}

// Get a current set of certs, then use them to sign a payload (experimental)
// Format is:
// uint8 - format version. Version 0 is defined as:
// uint64 - big endian cert length
// certificate
// uint64 - big endian sig length
// signature
func signData(config *ClientAppConfiguration, msg []byte) ([]byte, error) {
	signer, cert, err := loadSigningKey(config)
	if err != nil {
		return nil, err
	}

	sig, err := signer.Sign(rand.Reader, msg)
	if err != nil {
		return nil, err
	}

	certData := cert.Marshal()
	sigData := sig.Blob

	var rv []byte

	rv = append(rv, 0x00)

	bb := make([]byte, 8)

	binary.BigEndian.PutUint64(bb, uint64(len(certData)))
	rv = append(rv, bb...)

	rv = append(rv, certData...)

	binary.BigEndian.PutUint64(bb, uint64(len(sigData)))
	rv = append(rv, bb...)

	rv = append(rv, sigData...)

	return rv, nil
}

func errIsClock(err error) bool {
	return err != nil && err.Error() == "Token used before issued"
}

func validateTokenWithRetryForClock(idToken, clientID, hostedDomain string, retries int) (string, error) {
	var rv string
	var err error
	for done, attempts := false, 0; !done; attempts++ {
		rv, err = ValidateIDToken(idToken, clientID, hostedDomain)
		if errIsClock(err) {
			if attempts < retries {
				log.Print("Token appears to have come from the future - retrying in 1 second.")
				time.Sleep(time.Second)
			} else {
				done = true
			}
		} else {
			done = true
		}
	}
	return rv, err
}

func ProcessClient(config *ClientAppConfiguration) error {
	err := ValidateMachineIsSuitable(config)
	if err != nil {
		return err
	}

	hd, err := homedir.Dir()
	if err != nil {
		return err
	}
	path := filepath.Join(hd, config.CredentialFileName)

	// First, try to load creds, and if we have none, go ahead and authorize us
	creds, err := LoadCreds(path)
	if err != nil {
		err = Reauthorize(config, path)
		if err != nil {
			return err
		}
		creds, err = LoadCreds(path)
		if err != nil {
			return err
		}
	}

	// Now that we have creds, try to get a valid ID token refreshing if needed
	email, err := validateTokenWithRetryForClock(creds.IDToken, config.ClientID, config.HostedDomain, 5)
	if err != nil {
		creds, err = SwapRefreshForTokens(config, creds.RefreshToken)
		if err != nil {
			return err
		}
		err = SaveCreds(path, creds)
		if err != nil {
			return err
		}
		email, err = validateTokenWithRetryForClock(creds.IDToken, config.ClientID, config.HostedDomain, 5)
		if err != nil {
			return err
		}
	}

	log.Print("Have valid ID token for: ", email)
	err = FetchCerts(config, creds.IDToken, filepath.Join(hd, ".ssh"), filepath.Join("~", ".ssh"))
	if err != nil {
		return err
	}

	return nil
}
