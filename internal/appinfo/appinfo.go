package appinfo

const (
	Name            = "TinyChatGo"
	ServerName      = "TinyChatGoServer"
	ServerShortName = "TCGS"
	Version         = "1.1.6"
	Tag             = "v1.1.6"
)

// ClientServerURL is deliberately compiled into the desktop clients. Keep this
// as a source constant so a release cannot be redirected by a build flag or a
// saved local setting.
const ClientServerURL = "https://39.97.183.32:5630"

// ClientAccessPassword is compiled into desktop clients and automatically
// submitted on the server's optional first-layer Web access page. Keep it
// empty when the server does not enable that gate; releases that protect the
// built-in server should set this to the same value as the server setting.
const ClientAccessPassword = ""
