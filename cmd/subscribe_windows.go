package cmd

const (
	scriptExt                      = "bat"
	scriptHeader                   = ""
	clientCommandDescriptionSuffix = `The default config file for all client commands is %AppData%\ntfy\client.yml.`
)

var (
	scriptLauncher = []string{"cmd.exe", "/Q", "/C"}
)

func defaultClientConfigFile() string {
	return defaultClientConfigFileWindows()
}
