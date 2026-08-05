//go:build !windows && !darwin && !client

package gui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tinychatgo/internal/database"
	"tinychatgo/internal/server"
)

// Run starts the headless server on platforms where the desktop management UI
// is unavailable. Keeping this entry point in gui lets the existing main
// package produce both the Windows GUI binary and a Linux daemon.
func Run(logo, _ []byte) error {
	flags := flag.NewFlagSet("tinychatgo-server", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	dataDir := flags.String("data-dir", envOr("TINYCHATGO_DATA_DIR", "."), "directory for the database and uploaded files")
	listen := flags.String("listen", envOr("TINYCHATGO_LISTEN", ":8080"), "HTTP listen address")
	httpsListen := flags.String("https-listen", envOr("TINYCHATGO_HTTPS_LISTEN", ":8443"), "HTTPS listen address; empty disables HTTPS")
	certFile := flags.String("tls-cert", os.Getenv("TINYCHATGO_TLS_CERT"), "TLS certificate file")
	keyFile := flags.String("tls-key", os.Getenv("TINYCHATGO_TLS_KEY"), "TLS private key file")
	password := flags.String("access-password", os.Getenv("TINYCHATGO_ACCESS_PASSWORD"), "optional web access password (prefer the environment variable)")
	adminPassword := flags.String("admin-password", os.Getenv("TINYCHATGO_ADMIN_PASSWORD"), "administrator password (required to enable /admin/)")
	adminListen := flags.String("admin-listen", envOr("TINYCHATGO_ADMIN_LISTEN", ":8881"), "dedicated administrator HTTP listen address")
	trustedProxies := flags.String("trusted-proxies", os.Getenv("TINYCHATGO_TRUSTED_PROXIES"), "trusted reverse proxy IPs/CIDRs")
	requireApproval := flags.Bool("require-approval", envBool("TINYCHATGO_REQUIRE_APPROVAL", false), "require administrator approval for new accounts")
	showUsers := flags.Bool("show-users", envBool("TINYCHATGO_SHOW_USERS", true), "show the user list")
	privateChat := flags.Bool("private-chat", envBool("TINYCHATGO_PRIVATE_CHAT", true), "allow private messages")
	allowGroups := flags.Bool("allow-groups", envBool("TINYCHATGO_ALLOW_GROUPS", true), "allow users to create groups")
	clientDownload := flags.Bool("client-download", envBool("TINYCHATGO_CLIENT_DOWNLOAD", true), "show desktop client download in the web portal")
	redirectHTTPS := flags.Bool("redirect-https", envBool("TINYCHATGO_REDIRECT_HTTPS", false), "redirect HTTP requests to HTTPS")
	if err := flags.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	absoluteDataDir, err := filepath.Abs(strings.TrimSpace(*dataDir))
	if err != nil {
		return fmt.Errorf("resolve data directory: %w", err)
	}
	if err = os.MkdirAll(absoluteDataDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	uploadDir := filepath.Join(absoluteDataDir, "chat_files")
	if err = os.MkdirAll(uploadDir, 0o750); err != nil {
		return fmt.Errorf("create upload directory: %w", err)
	}
	if strings.TrimSpace(*httpsListen) != "" {
		if (*certFile == "") != (*keyFile == "") {
			return errors.New("HTTPS requires both -tls-cert and -tls-key, or neither for an automatically generated certificate")
		}
		if *certFile == "" {
			certificate, generateErr := generateHTTPSCertificate(absoluteDataDir, linuxCertificateHosts())
			if generateErr != nil {
				return fmt.Errorf("generate HTTPS certificate: %w", generateErr)
			}
			*certFile, *keyFile = certificate.CertPath, certificate.KeyPath
			log.Printf("generated/reused self-signed HTTPS certificate; CA certificate: %s", certificate.CAPath)
		}
	}

	store, err := database.Open(filepath.Join(absoluteDataDir, "tinychatgo.db"))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()
	storedSettings := defaultPersistedSettings()
	settingsFound, settingsErr := store.LoadSettings(&storedSettings)
	if settingsErr != nil {
		return fmt.Errorf("load settings: %w", settingsErr)
	}
	if settingsFound {
		if os.Getenv("TINYCHATGO_REQUIRE_APPROVAL") == "" {
			*requireApproval = storedSettings.RequireAccountApproval
		}
		if os.Getenv("TINYCHATGO_ALLOW_GROUPS") == "" {
			*allowGroups = storedSettings.AllowGroupChat
		}
		if os.Getenv("TINYCHATGO_SHOW_USERS") == "" {
			*showUsers = storedSettings.ShowUserList
		}
		if os.Getenv("TINYCHATGO_PRIVATE_CHAT") == "" {
			*privateChat = storedSettings.AllowPrivateChat
		}
		if os.Getenv("TINYCHATGO_CLIENT_DOWNLOAD") == "" {
			*clientDownload = storedSettings.AllowClientDownload
		}
	}

	srv := server.New(os.Stdout)
	srv.SetBrandLogo(logo)
	srv.SetAccess(*password, false, false, false)
	srv.SetAdminPassword(*adminPassword)
	srv.SetAdminSettingsSaver(func(settings server.AdminSettings) error {
		storedSettings.RequireAccountApproval = settings.RequireApproval
		storedSettings.AllowGroupChat = settings.AllowGroups
		storedSettings.ShowUserList = settings.ShowUsers
		storedSettings.AllowPrivateChat = settings.PrivateChat && settings.ShowUsers
		storedSettings.AllowClientDownload = *clientDownload
		return store.SaveSettings(storedSettings)
	})
	if err = srv.SetTrustedProxies(*trustedProxies); err != nil {
		return err
	}
	srv.SetChatEnabled(true)
	srv.SetUserGroupCreationEnabled(*allowGroups)
	srv.SetUserListEnabled(*showUsers)
	srv.SetPrivateMessagesEnabled(*privateChat && *showUsers)
	srv.SetAccountApprovalRequired(*requireApproval)
	srv.SetClientDownloadEnabled(*clientDownload)
	if err = srv.SetFallbackUploadDir(uploadDir); err != nil {
		return err
	}
	if err = srv.SetPersistence(store); err != nil {
		return fmt.Errorf("restore persistent data: %w", err)
	}

	var shares []server.Share
	if found, loadErr := store.LoadShares(&shares); loadErr != nil {
		return fmt.Errorf("load shares: %w", loadErr)
	} else if found {
		srv.ReplaceShares(shares)
	}
	srv.SetShareChangeNotifier(func() {
		if saveErr := store.SaveShares(srv.Shares()); saveErr != nil {
			log.Printf("save shares: %v", saveErr)
		}
	})

	var adminHTTP *http.Server
	if *adminPassword != "" {
		adminListener, listenErr := net.Listen("tcp", strings.TrimSpace(*adminListen))
		if listenErr != nil {
			return fmt.Errorf("listen administrator HTTP on %q: %w", *adminListen, listenErr)
		}
		adminHTTP = &http.Server{Handler: srv.AdminHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		go func() {
			if serveErr := adminHTTP.Serve(adminListener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				log.Printf("administrator HTTP server error: %v", serveErr)
			}
		}()
		log.Printf("TinyChatGo administrator server listening on %s", adminListener.Addr())
	}

	if *redirectHTTPS {
		host, port := redirectTarget(*httpsListen)
		if host == "" || port == "" {
			return errors.New("-redirect-https requires a valid -https-listen address")
		}
		srv.SetHTTPSRedirect(true, host, port)
	}
	addresses, err := srv.StartWithHTTPS(*listen, *httpsListen, *certFile, *keyFile)
	if err != nil {
		if adminHTTP != nil {
			_ = adminHTTP.Close()
		}
		return err
	}
	log.Printf("TinyChatGo HTTP server listening on %s", addresses.HTTP)
	if addresses.HTTPS != "" {
		log.Printf("TinyChatGo HTTPS server listening on %s", addresses.HTTPS)
	}
	log.Printf("data directory: %s", absoluteDataDir)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	signal.Stop(stop)
	log.Print("shutting down TinyChatGo")
	serverErr := srv.Stop()
	if adminHTTP != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		adminErr := adminHTTP.Shutdown(ctx)
		cancel()
		return errors.Join(serverErr, adminErr)
	}
	return serverErr
}

func RunClient(_ []byte) error { return errors.New("desktop client is not supported on this platform") }

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func redirectTarget(address string) (string, string) {
	parsed, err := url.Parse("//" + strings.TrimSpace(address))
	if err != nil {
		return "", ""
	}
	host := parsed.Hostname()
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return host, parsed.Port()
}

func linuxCertificateHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if hostname, err := os.Hostname(); err == nil && strings.TrimSpace(hostname) != "" {
		hosts = append(hosts, hostname)
	}
	if addresses, err := net.InterfaceAddrs(); err == nil {
		for _, address := range addresses {
			if network, ok := address.(*net.IPNet); ok && network.IP != nil {
				hosts = append(hosts, network.IP.String())
			}
		}
	}
	return hosts
}
