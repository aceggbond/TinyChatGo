//go:build windows

package gui

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"lanchatgo/internal/database"
	"lanchatgo/internal/server"
)

func TestModernControllerSettingsAndServerLifecycle(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}

	temp := t.TempDir()
	logBuffer := &safeBuffer{}
	controller := &modernController{
		srv:          server.New(io.Discard),
		log:          logBuffer,
		settings:     defaultPersistedSettings(),
		settingsPath: filepath.Join(temp, settingsFileName),
		sharesPath:   filepath.Join(temp, "hfsgo.json"),
		addresses:    []modernAddress{{Host: "127.0.0.1", Label: "127.0.0.1（本机）"}},
	}
	settings := defaultPersistedSettings()
	settings.Password = "secret"
	settings.AllowUpload = true
	settings.AllowChat = true
	settings.NotifyNewVisitor = false
	settings.AccessHost = "127.0.0.1"
	settings.Port = strconv.Itoa(port)
	state, err := controller.saveSettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	if state.Settings != settings {
		t.Fatalf("settings not applied: %#v", state.Settings)
	}

	state, err = controller.toggleServer(settings.AccessHost, settings.Port, "")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Running || state.Address == "" {
		t.Fatalf("server did not start: %#v", state)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	response, err := client.Get(state.Address)
	if err != nil {
		t.Fatalf("running server was unreachable: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("password-protected root status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}

	state, err = controller.toggleServer(settings.AccessHost, settings.Port, "")
	if err != nil {
		t.Fatal(err)
	}
	if state.Running {
		t.Fatal("server remained running after stop")
	}
}

func TestRemoveLegacyDataFileStaysInsideApplicationDirectory(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, settingsFileName)
	if err := os.WriteFile(legacy, []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyDataFile(legacy, root, settingsFileName); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file still exists: %v", err)
	}
	outside := filepath.Join(t.TempDir(), settingsFileName)
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeLegacyDataFile(outside, root, settingsFileName); err == nil {
		t.Fatal("unsafe legacy path was accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unsafe legacy file was changed: %v", err)
	}
}

func TestValidateModernSettingsHTTPSPort(t *testing.T) {
	addresses := []modernAddress{{Host: "127.0.0.1"}}
	settings := defaultPersistedSettings()
	settings.AccessHost = "127.0.0.1"
	settings.HTTPSPort = "1123"
	if _, err := validateModernSettings(settings, addresses); err != nil {
		t.Fatal(err)
	}
	settings.HTTPSPort = settings.Port
	if _, err := validateModernSettings(settings, addresses); err == nil {
		t.Fatal("matching HTTP and HTTPS ports were accepted")
	}
	settings.HTTPSPort = "70000"
	if _, err := validateModernSettings(settings, addresses); err == nil {
		t.Fatal("out-of-range HTTPS port was accepted")
	}
	settings.HTTPSPort = ""
	settings.RedirectToHTTPS = true
	if _, err := validateModernSettings(settings, addresses); err == nil {
		t.Fatal("HTTP redirect without an HTTPS port was accepted")
	}
}

func TestNativeDropPaintStructMatchesWindowsLayout(t *testing.T) {
	if size := unsafe.Sizeof(modernPaintStruct{}); size != 72 {
		t.Fatalf("PAINTSTRUCT size = %d, want 72", size)
	}
}

func TestModernShareDropIsLimitedToFilePage(t *testing.T) {
	controller := &modernController{activePage: "chat"}
	if controller.acceptsShareDrop() {
		t.Fatal("chat page accepted a file-management drop")
	}
	controller.setActivePage("files")
	if !controller.acceptsShareDrop() {
		t.Fatal("file page rejected a file-management drop")
	}
	controller.setActivePage("settings")
	if controller.acceptsShareDrop() {
		t.Fatal("settings page accepted a file-management drop")
	}
}

func TestWindowsTemporaryUploadDirUsesSystemRoot(t *testing.T) {
	t.Setenv("SystemRoot", `C:\Windows`)
	if got := windowsTemporaryUploadDir(); got != `C:\Windows\Temp` {
		t.Fatalf("temporary upload directory = %q", got)
	}
}

func TestModernControllerTemporaryShareDeletion(t *testing.T) {
	root := t.TempDir()
	temporaryDir := filepath.Join(root, "Temp")
	if err := os.Mkdir(temporaryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	temporaryFile := filepath.Join(temporaryDir, "browser-upload.txt")
	manualTemporaryShare := filepath.Join(temporaryDir, "manually-shared.txt")
	regularFile := filepath.Join(root, "normal-share.txt")
	if err := os.WriteFile(temporaryFile, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(regularFile, []byte("normal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manualTemporaryShare, []byte("manual"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := server.New(io.Discard)
	if err := srv.AddManagedTemporary(temporaryFile); err != nil {
		t.Fatal(err)
	}
	if err := srv.Add(regularFile); err != nil {
		t.Fatal(err)
	}
	if err := srv.Add(manualTemporaryShare); err != nil {
		t.Fatal(err)
	}
	controller := &modernController{
		srv:           srv,
		log:           &safeBuffer{},
		settings:      defaultPersistedSettings(),
		settingsPath:  filepath.Join(root, settingsFileName),
		sharesPath:    filepath.Join(root, "hfsgo.json"),
		tempUploadDir: temporaryDir,
	}
	if _, err := controller.removeShares([]int{0, 1, 2}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporaryFile); !os.IsNotExist(err) {
		t.Fatalf("temporary upload still exists: %v", err)
	}
	if _, err := os.Stat(regularFile); err != nil {
		t.Fatalf("normal shared file was deleted: %v", err)
	}
	if _, err := os.Stat(manualTemporaryShare); err != nil {
		t.Fatalf("manually shared Temp file was deleted: %v", err)
	}
	if shares := srv.Shares(); len(shares) != 0 {
		t.Fatalf("shares remain after removal: %#v", shares)
	}
}

func TestPathInsideDirectoryRejectsDirectoryItselfAndSibling(t *testing.T) {
	root := t.TempDir()
	if pathInsideDirectory(root, root) {
		t.Fatal("directory itself was treated as a managed child")
	}
	if !pathInsideDirectory(filepath.Join(root, "file.txt"), root) {
		t.Fatal("child file was not recognized")
	}
	if pathInsideDirectory(filepath.Join(filepath.Dir(root), "sibling", "file.txt"), root) {
		t.Fatal("sibling path was treated as a managed child")
	}
	if !pathDirectlyInsideDirectory(filepath.Join(root, "file.txt"), root) {
		t.Fatal("direct child was not recognized as a managed temporary upload")
	}
	if pathDirectlyInsideDirectory(filepath.Join(root, "junction", "file.txt"), root) {
		t.Fatal("nested path expanded the managed physical-deletion boundary")
	}
}

func TestModernControllerGeneratesCertificateAndStartsHTTPS(t *testing.T) {
	httpPort, httpsPort := adjacentFreePorts(t)
	temp := t.TempDir()
	settings := defaultPersistedSettings()
	settings.AccessHost = "127.0.0.1"
	settings.Port = strconv.Itoa(httpPort)
	controller := &modernController{
		srv:            server.New(io.Discard),
		log:            &safeBuffer{},
		settings:       settings,
		settingsPath:   filepath.Join(temp, settingsFileName),
		sharesPath:     filepath.Join(temp, "hfsgo.json"),
		certificateDir: temp,
		addresses:      []modernAddress{{Host: "127.0.0.1", Label: "127.0.0.1（本机）"}},
	}
	state, err := controller.generateCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Certificate.Available || state.Settings.HTTPSPort != strconv.Itoa(httpsPort) {
		t.Fatalf("generated state = %#v", state)
	}
	redirectSettings := state.Settings
	redirectSettings.RedirectToHTTPS = true
	state, err = controller.saveSettings(redirectSettings)
	if err != nil {
		t.Fatal(err)
	}
	state, err = controller.toggleServer("127.0.0.1", strconv.Itoa(httpPort), strconv.Itoa(httpsPort))
	if err != nil {
		t.Fatal(err)
	}
	defer controller.srv.Stop()
	if !state.Running || state.Address == "" || state.HTTPSAddress == "" {
		t.Fatalf("dual-listener state = %#v", state)
	}
	noRedirectClient := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	redirectResponse, err := noRedirectClient.Get(state.Address + "/shared?q=one")
	if err != nil {
		t.Fatal(err)
	}
	_ = redirectResponse.Body.Close()
	if redirectResponse.StatusCode != http.StatusTemporaryRedirect || redirectResponse.Header.Get("Location") != state.HTTPSAddress+"/shared?q=one" {
		t.Fatalf("HTTP redirect = %d %q", redirectResponse.StatusCode, redirectResponse.Header.Get("Location"))
	}

	caPEM, err := os.ReadFile(state.Certificate.CAPath)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("generated CA could not be loaded")
	}
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    roots,
			MinVersion: tls.VersionTLS12,
		}},
	}
	response, err := client.Get(state.HTTPSAddress)
	if err != nil {
		t.Fatalf("generated HTTPS listener was unreachable: %v", err)
	}
	_ = response.Body.Close()
	client.CloseIdleConnections()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("HTTPS root status = %d", response.StatusCode)
	}
	if _, err = controller.toggleServer("127.0.0.1", strconv.Itoa(httpPort), strconv.Itoa(httpsPort)); err != nil {
		t.Fatal(err)
	}
}

func TestModernControllerStoresCertificateOnlyInDatabase(t *testing.T) {
	temp := t.TempDir()
	store, err := database.Open(filepath.Join(temp, "hfs-go.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	settings := defaultPersistedSettings()
	settings.AccessHost = "127.0.0.1"
	controller := &modernController{
		srv: server.New(io.Discard), db: store, log: &safeBuffer{},
		settings: settings, databasePath: store.Path(), certificateDir: temp,
		addresses: []modernAddress{{Host: "127.0.0.1", Label: "127.0.0.1（本机）"}},
	}
	state, err := controller.generateCertificate()
	if err != nil {
		t.Fatal(err)
	}
	if !state.Certificate.Available || state.Certificate.CAPath != "hfs-go.db" {
		t.Fatalf("database certificate status = %#v", state.Certificate)
	}
	if _, found, loadErr := store.LoadCertificateBundle(); loadErr != nil || !found {
		t.Fatalf("certificate bundle was not stored: found=%v error=%v", found, loadErr)
	}
	for _, name := range []string{httpsCAFileName, httpsCAKeyFileName, httpsCertFileName, httpsCertKeyFileName} {
		if _, statErr := os.Stat(filepath.Join(temp, name)); !os.IsNotExist(statErr) {
			t.Fatalf("certificate sidecar %s still exists: %v", name, statErr)
		}
	}
}

func adjacentFreePorts(t *testing.T) (int, int) {
	t.Helper()
	for attempt := 0; attempt < 100; attempt++ {
		first, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := first.Addr().(*net.TCPAddr).Port
		if port >= 65535 {
			_ = first.Close()
			continue
		}
		second, secondErr := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port+1)))
		_ = first.Close()
		if secondErr != nil {
			continue
		}
		_ = second.Close()
		return port, port + 1
	}
	t.Fatal("could not find adjacent free ports")
	return 0, 0
}
