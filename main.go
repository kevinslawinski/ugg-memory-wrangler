// ugg-memory-wrangler – kill and restart U.GG (or any process) on Windows.
//
// Usage:
//
//	ugg-memory-wrangler.exe                          # auto-detect path, kill & restart ugg.exe
//	ugg-memory-wrangler.exe -name ugg.exe            # explicit process name
//	ugg-memory-wrangler.exe -path "C:\...\ugg.exe"   # explicit executable path
//	ugg-memory-wrangler.exe -delay 3s                # wait 3 s before restarting
//	ugg-memory-wrangler.exe -kill-only               # terminate without restarting
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const (
	defaultProcessName = "U.GG.exe"
	exitTimeout        = 10 * time.Second
	pollInterval       = 500 * time.Millisecond
	mbOK               = 0x00000000
	mbIconInfo         = 0x00000040
	mbIconError        = 0x00000010
)

type configData struct {
	ExePath string `json:"exePath"`
}

type metrics struct {
	Runs            int    `json:"runs"`
	TotalFreedBytes int64  `json:"totalFreedBytes"`
	LastFreedBytes  int64  `json:"lastFreedBytes"`
	LastBeforeBytes int64  `json:"lastBeforeBytes"`
	UpdatedAt       string `json:"updatedAt"`
}

// progressState is written to a temp JSON file that the live WPF window polls every 250ms.
type progressState struct {
	Status   string `json:"status"` // "working" | "done"
	Before   string `json:"before"`
	Stopping string `json:"stopping"` // "" | "done" | "n/a"
	Starting string `json:"starting"` // "" | "done"
	Warmup   string `json:"warmup"`   // "" | "done"
	After    string `json:"after"`
	Freed    string `json:"freed"`
	FreedPos bool   `json:"freedPos"`
	Total    string `json:"total"`
	Avg      string `json:"avg"`
	Message  string `json:"message"` // shown in red when non-empty
}

func writeProgress(path string, s progressState) {
	data, _ := json.Marshal(s)
	_ = os.WriteFile(path, data, 0o644)
}

// liveWindowXAML returns the WPF XAML for the live progress/result window.
func liveWindowXAML() string {
	col := `<Grid.ColumnDefinitions><ColumnDefinition Width="*"/><ColumnDefinition MinWidth="140"/></Grid.ColumnDefinitions>`
	makeRow := func(name, label, valColor string) string {
		return fmt.Sprintf(
			`<Grid x:Name="row%s" Visibility="Collapsed" Margin="0,3,0,3">%s`+
				`<TextBlock Text="%s" Foreground="#888888" FontSize="13"/>`+
				`<TextBlock x:Name="val%s" Grid.Column="1" Foreground="%s" FontSize="13" TextAlignment="Right"/>`+
				`</Grid>`,
			name, col, label, name, valColor,
		)
	}
	grey, teal := "#cccccc", "#4ec9b0"
	steps := makeRow("Before", "Before restart", grey) +
		makeRow("Stopping", "Stopping U.GG", grey) +
		makeRow("Starting", "Starting U.GG", grey) +
		makeRow("Warmup", "Letting U.GG settle", grey) +
		makeRow("After", "After restart", grey)
	results := makeRow("FreedTeal", "Freed this run", teal) +
		makeRow("FreedGrey", "Result", grey) +
		makeRow("Total", "Total freed", grey) +
		makeRow("Avg", "Avg. per restart", grey)
	return `<Window xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation" ` +
		`xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml" ` +
		`Title="U.GG Memory Wrangler" Width="420" SizeToContent="Height" ` +
		`WindowStartupLocation="CenterScreen" ResizeMode="NoResize" ` + `ShowInTaskbar="True" ` + `Background="#1e1e1e" FontFamily="Segoe UI">` +
		`<Border Padding="24,20">` +
		`<StackPanel>` +
		`<TextBlock Text="U.GG Memory Wrangler" FontSize="17" FontWeight="SemiBold" Foreground="#ffffff"/>` +
		`<Rectangle Fill="#0078d4" Height="2" Margin="0,8,0,16"/>` +
		steps +
		`<TextBlock x:Name="lblWorking" Text="Working..." Foreground="#555555" FontSize="12" Margin="0,10,0,0"/>` +
		`<StackPanel x:Name="pnlResult" Visibility="Collapsed">` +
		`<Rectangle Fill="#333333" Height="1" Margin="0,12,0,8"/>` +
		results +
		`<TextBlock x:Name="lblMessage" Foreground="#e07b7b" FontSize="12" Margin="0,8,0,0" Visibility="Collapsed" TextWrapping="Wrap"/>` +
		`</StackPanel>` +
		`<Rectangle Fill="#333333" Height="1" Margin="0,16,0,4"/>` +
		`<DockPanel LastChildFill="False" Margin="0,4,0,0">` +
		`<Button x:Name="btnLog" Content="Open Log" Width="90" Height="30" Background="#2d2d2d" Foreground="#888888" BorderThickness="0" FontSize="13" Cursor="Hand"/>` +
		`<Button x:Name="btnClose" Content="Close" Width="90" Height="30" DockPanel.Dock="Right" Background="#0078d4" Foreground="White" BorderThickness="0" FontSize="13" Cursor="Hand" IsEnabled="False"/>` +
		`</DockPanel>` +
		`</StackPanel>` +
		`</Border>` +
		`</Window>`
}

// showLiveWindow writes XAML to a temp file then starts a PowerShell WPF process
// that polls statusFile every 250ms, animating steps in as they complete.
// Returns immediately; the window stays open until the user clicks Close.
func showLiveWindow(statusFile, logPath string) {
	xamlPath := filepath.Join(os.TempDir(), "ugg-memory-wrangler-ui.xaml")
	if err := os.WriteFile(xamlPath, []byte(liveWindowXAML()), 0o644); err != nil {
		return
	}
	logClick := `$w.FindName('btnLog').Visibility = 'Collapsed'; `
	if logPath != "" {
		logClick = fmt.Sprintf(`$w.FindName('btnLog').Add_Click({Start-Process '%s'}); `,
			strings.ReplaceAll(logPath, "'", "''"))
	}
	psScript := fmt.Sprintf(
		`Add-Type -AssemblyName PresentationFramework; `+
			`[xml]$x = Get-Content '%s' -Raw -Encoding UTF8; `+
			`$r = [System.Xml.XmlNodeReader]::new($x); `+
			`$w = [Windows.Markup.XamlReader]::Load($r); `+
			`$sf = '%s'; $script:dc = 0; `+
			`$timer = New-Object System.Windows.Threading.DispatcherTimer; `+
			`$timer.Interval = [TimeSpan]::FromMilliseconds(250); `+
			`$timer.Add_Tick({ `+
			`$script:dc = ($script:dc %% 3) + 1; `+
			`try { if (Test-Path $sf) { `+
			`$d = Get-Content $sf -Raw -ErrorAction Stop | ConvertFrom-Json; `+
			`if ($d.before -ne '') { $w.FindName('rowBefore').Visibility='Visible'; $w.FindName('valBefore').Text=$d.before }; `+
			`if ($d.stopping -ne '') { $w.FindName('rowStopping').Visibility='Visible'; $w.FindName('valStopping').Text=$d.stopping }; `+
			`if ($d.starting -ne '') { $w.FindName('rowStarting').Visibility='Visible'; $w.FindName('valStarting').Text=$d.starting }; `+
			`if ($d.warmup -ne '') { $w.FindName('rowWarmup').Visibility='Visible'; $w.FindName('valWarmup').Text=$d.warmup }; `+
			`if ($d.after -ne '') { $w.FindName('rowAfter').Visibility='Visible'; $w.FindName('valAfter').Text=$d.after }; `+
			`if ($d.status -eq 'done') { `+
			`$timer.Stop(); `+
			`$w.FindName('lblWorking').Visibility='Collapsed'; `+
			`if ($d.freed -ne '') { if ($d.freedPos) { $w.FindName('rowFreedTeal').Visibility='Visible'; $w.FindName('valFreedTeal').Text=$d.freed } else { $w.FindName('rowFreedGrey').Visibility='Visible'; $w.FindName('valFreedGrey').Text=$d.freed } }; `+
			`if ($d.total -ne '') { $w.FindName('rowTotal').Visibility='Visible'; $w.FindName('valTotal').Text=$d.total }; `+
			`if ($d.avg -ne '') { $w.FindName('rowAvg').Visibility='Visible'; $w.FindName('valAvg').Text=$d.avg }; `+
			`if ($d.message -ne '') { $w.FindName('lblMessage').Text=$d.message; $w.FindName('lblMessage').Visibility='Visible' }; `+
			`$w.FindName('pnlResult').Visibility='Visible'; `+
			`$w.FindName('btnClose').IsEnabled=$true `+
			`} else { $w.FindName('lblWorking').Text = 'Working' + ('.' * $script:dc) } `+
			`} } catch {} `+
			`}); `+
			`$timer.Start(); `+
			`$w.Add_ContentRendered({$w.Activate()}); `+
			`%s`+
			`$w.FindName('btnClose').Add_Click({$timer.Stop(); $w.Close(); Remove-Item $sf -EA SilentlyContinue; Remove-Item '%s' -EA SilentlyContinue}); `+
			`[void]$w.ShowDialog()`,
		strings.ReplaceAll(xamlPath, "'", "''"),
		strings.ReplaceAll(statusFile, "'", "''"),
		logClick,
		strings.ReplaceAll(xamlPath, "'", "''"),
	)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

func psSingleQuote(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

// hiddenCmd creates a command that runs without creating a visible window.
// All subprocess invocations must go through this to avoid CMD/PowerShell flashes.
func hiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd
}

func showMessageBox(title string, body string, isError bool) {
	user32 := syscall.NewLazyDLL("user32.dll")
	messageBoxW := user32.NewProc("MessageBoxW")
	flags := uintptr(mbOK | mbIconInfo)
	if isError {
		flags = uintptr(mbOK | mbIconError)
	}
	_, _, _ = messageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(body))),
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr(title))),
		flags,
	)
}

func xmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return s
}

// showToast fires a Windows toast notification via PowerShell WinRT.
// No external dependencies — uses the PowerShell AppID as a registration workaround.
func showToast(title string, lines []string) error {
	appID := `UGGMemWrangler.App`

	var textElems strings.Builder
	for _, line := range lines {
		if line == "" {
			continue
		}
		fmt.Fprintf(&textElems, "<text>%s</text>", xmlEscape(line))
	}

	xmlDoc := fmt.Sprintf(
		`<toast duration="long"><visual><binding template="ToastGeneric">%s</binding></visual></toast>`,
		textElems.String(),
	)

	psCmd := fmt.Sprintf(
		`[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null; `+
			`[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null; `+
			`$xml = New-Object Windows.Data.Xml.Dom.XmlDocument; `+
			`$xml.LoadXml('%s'); `+
			`$toast = [Windows.UI.Notifications.ToastNotification]::new($xml); `+
			`[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('%s').Show($toast)`,
		strings.ReplaceAll(xmlDoc, "'", "''"),
		appID,
	)

	cmd := hiddenCmd("powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("toast: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// notify sends a toast notification, falling back to a MessageBox if toast fails.
func notify(title string, lines []string, isError bool) {
	if err := showToast(title, lines); err != nil {
		showMessageBox(title, strings.Join(lines, "\n"), isError)
	}
}

// statusRow is one line in the WPF status window.
type statusRow struct {
	Label     string
	Value     string
	Separator bool // render as a horizontal rule instead of a label/value pair
	Highlight bool // green accent for positive outcomes
}

// showStatusWindow displays a compact WPF window summarising the run result.
// It launches a hidden PowerShell process and returns immediately (non-blocking).
// logPath is optional; when set an "Open Log" button is shown.
func showStatusWindow(header string, rows []statusRow, isError bool, logPath string) {
	accentColor := "#0078d4"
	if isError {
		accentColor = "#c0392b"
	}

	var rowsXML strings.Builder
	for _, r := range rows {
		if r.Separator {
			rowsXML.WriteString(`<Rectangle Fill="#333333" Height="1" Margin="0,8,0,8"/>`)
			continue
		}
		valColor := "#cccccc"
		if r.Highlight {
			valColor = "#4ec9b0"
		}
		fmt.Fprintf(&rowsXML,
			`<Grid Margin="0,4,0,0">`+
				`<Grid.ColumnDefinitions>`+
				`<ColumnDefinition Width="*"/>`+
				`<ColumnDefinition MinWidth="130"/>`+
				`</Grid.ColumnDefinitions>`+
				`<TextBlock Text="%s" Foreground="#888888" FontSize="13"/>`+
				`<TextBlock Grid.Column="1" Text="%s" Foreground="%s" FontSize="13" TextAlignment="Right"/>`+
				`</Grid>`,
			xmlEscape(r.Label), xmlEscape(r.Value), valColor,
		)
	}

	// Footer: Close button always present; Open Log button shown when logPath is set.
	var footerXAML strings.Builder
	footerXAML.WriteString(`<DockPanel LastChildFill="False" Margin="0,12,0,0">`)
	if logPath != "" {
		footerXAML.WriteString(
			`<Button x:Name="btnLog" Content="Open Log" Width="90" Height="30" ` +
				`Background="#2d2d2d" Foreground="#888888" BorderThickness="0" FontSize="13" Cursor="Hand"/>`,
		)
	}
	fmt.Fprintf(&footerXAML,
		`<Button x:Name="btnClose" Content="Close" Width="90" Height="30" `+
			`DockPanel.Dock="Right" Background="%s" Foreground="White" `+
			`BorderThickness="0" FontSize="13" Cursor="Hand"/>`,
		accentColor,
	)
	footerXAML.WriteString(`</DockPanel>`)

	xaml := fmt.Sprintf(
		`<Window xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation" `+
			`xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml" `+
			`Title="U.GG Memory Wrangler" Width="400" SizeToContent="Height" `+
			`WindowStartupLocation="CenterScreen" ResizeMode="NoResize" `+
			`Background="#1e1e1e" FontFamily="Segoe UI">`+
			`<Border Padding="24,20">`+
			`<StackPanel>`+
			`<TextBlock Text="%s" FontSize="17" FontWeight="SemiBold" Foreground="#ffffff"/>`+
			`<Rectangle Fill="%s" Height="2" Margin="0,8,0,16"/>`+
			`%s`+
			`<Rectangle Fill="#333333" Height="1" Margin="0,16,0,4"/>`+
			`%s`+
			`</StackPanel>`+
			`</Border>`+
			`</Window>`,
		xmlEscape(header), accentColor, rowsXML.String(), footerXAML.String(),
	)

	var logClickHandler string
	if logPath != "" {
		logClickHandler = fmt.Sprintf(
			`$w.FindName('btnLog').Add_Click({Start-Process '%s'}); `,
			strings.ReplaceAll(logPath, "'", "''"),
		)
	}

	psScript := fmt.Sprintf(
		`Add-Type -AssemblyName PresentationFramework; `+
			`[xml]$x = '%s'; `+
			`$r = [System.Xml.XmlNodeReader]::new($x); `+
			`$w = [Windows.Markup.XamlReader]::Load($r); `+
			`%s`+
			`$w.FindName('btnClose').Add_Click({$w.Close()}); `+
			`[void]$w.ShowDialog()`,
		strings.ReplaceAll(xaml, "'", "''"),
		logClickHandler,
	)

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Start()
}

// ensureToastAppID registers a custom AppUserModelID in HKCU so that toast
// notifications display "U.GG Memory Wrangler" instead of "Windows PowerShell".
func ensureToastAppID() {
	psScript := `$path = 'HKCU:\SOFTWARE\Classes\AppUserModelId\UGGMemWrangler.App'; ` +
		`if (-not (Test-Path $path)) { New-Item -Path $path -Force | Out-Null }; ` +
		`Set-ItemProperty -Path $path -Name 'DisplayName' -Value 'U.GG Memory Wrangler' -Type String`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", psScript)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}

// getExePath returns the full disk path of a running process using PowerShell.
// It must be called before the process is killed so the path is still available.
func getExePath(processName string) string {
	psName := strings.TrimSuffix(processName, ".exe")
	out, err := hiddenCmd(
		"powershell", "-NoProfile", "-NonInteractive", "-Command",
		fmt.Sprintf(
			`(Get-Process '%s' -ErrorAction SilentlyContinue | Select-Object -First 1).Path`,
			psSingleQuote(psName),
		),
	).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// getExePathByKeyword finds a running process path by matching a keyword.
func getExePathByKeyword(keyword string) string {
	if keyword == "" {
		return ""
	}
	cmd := fmt.Sprintf(
		`$p=(Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.Path -and $_.ProcessName -match '(?i)%s' } | Select-Object -First 1 -ExpandProperty Path); if ($p) { $p }`,
		psSingleQuote(keyword),
	)
	out, err := hiddenCmd(
		"powershell", "-NoProfile", "-NonInteractive", "-Command", cmd,
	).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func firstMatchingExe(paths []string, keyword string) string {
	needle := strings.ToLower(keyword)
	for _, p := range paths {
		if p == "" {
			continue
		}
		lower := strings.ToLower(p)
		if !strings.Contains(lower, needle) {
			continue
		}
		if strings.Contains(lower, "update") || strings.Contains(lower, "uninstall") {
			continue
		}
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// findExeInCommonLocations scans likely install directories for a matching exe.
func findExeInCommonLocations(keyword string) string {
	if keyword == "" {
		return ""
	}

	bases := []string{
		os.Getenv("LOCALAPPDATA"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs"),
		os.Getenv("PROGRAMFILES"),
		os.Getenv("PROGRAMFILES(X86)"),
	}

	if exeSelf, err := os.Executable(); err == nil {
		bases = append(bases, filepath.Dir(exeSelf))
	}

	seenBase := make(map[string]bool)
	patterns := make([]string, 0, 24)
	for _, base := range bases {
		if base == "" {
			continue
		}
		clean := filepath.Clean(base)
		if seenBase[clean] {
			continue
		}
		seenBase[clean] = true

		patterns = append(patterns,
			filepath.Join(clean, keyword+"*.exe"),
			filepath.Join(clean, "*"+keyword+"*", "*.exe"),
			filepath.Join(clean, "*", "*"+keyword+"*", "*.exe"),
		)
	}

	for _, pattern := range patterns {
		if matches, err := filepath.Glob(pattern); err == nil {
			if candidate := firstMatchingExe(matches, keyword); candidate != "" {
				return candidate
			}
		}
	}

	return ""
}

func getConfigPath() string {
	return filepath.Join(defaultDataDir(), "config.json")
}

func loadConfig() configData {
	var cfg configData
	bytes, err := os.ReadFile(getConfigPath())
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(bytes, &cfg)
	return cfg
}

func saveConfig(cfg configData) error {
	if err := ensureDir(defaultDataDir()); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(getConfigPath(), bytes, 0o644)
}

// scanDeepForExe recursively searches up to 3 levels deep for matching executables.
func scanDeepForExe(rootDir string, keyword string, maxDepth int) []string {
	var results []string
	if rootDir == "" || maxDepth < 0 {
		return results
	}
	entries, _ := os.ReadDir(rootDir)
	for _, e := range entries {
		if len(results) >= 20 {
			break
		}
		if strings.EqualFold(e.Name(), keyword) && !e.IsDir() {
			results = append(results, filepath.Join(rootDir, e.Name()))
		} else if e.IsDir() && maxDepth > 0 && !strings.HasPrefix(e.Name(), ".") {
			results = append(results, scanDeepForExe(filepath.Join(rootDir, e.Name()), keyword, maxDepth-1)...)
		}
	}
	return results
}

func resolveExePath(processName string, explicitPath string) (string, string) {
	if explicitPath != "" {
		return normalizeExePath(explicitPath), "explicit -path"
	}
	if path := getExePath(processName); path != "" {
		return normalizeExePath(path), "running process name lookup"
	}

	keyword := strings.TrimSuffix(strings.ToLower(processName), ".exe")
	if path := getExePathByKeyword(keyword); path != "" {
		return normalizeExePath(path), "running process keyword lookup"
	}

	cfg := loadConfig()
	if cfg.ExePath != "" {
		if _, err := os.Stat(cfg.ExePath); err == nil {
			return normalizeExePath(cfg.ExePath), "saved config path"
		}
	}

	bases := []string{
		os.Getenv("LOCALAPPDATA"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "EdgeApps"),
		os.Getenv("PROGRAMFILES"),
		os.Getenv("PROGRAMFILES(X86)"),
	}

	for _, base := range bases {
		if base == "" {
			continue
		}
		if matches := scanDeepForExe(base, "U.GG.exe", 3); len(matches) > 0 {
			if candidate := firstMatchingExe(matches, keyword); candidate != "" {
				return normalizeExePath(candidate), "deep directory scan"
			}
		}
	}

	if path := findExeInCommonLocations(keyword); path != "" {
		return normalizeExePath(path), "common install location scan"
	}

	return "", ""
}

func normalizeExePath(path string) string {
	clean := strings.TrimSpace(path)
	clean = strings.Trim(clean, `"`)
	clean = filepath.FromSlash(clean)
	return clean
}

// getProcessMemoryBytes returns summed "Working Set - Private" bytes for all process instances.
// This matches the Task Manager Processes tab memory figure much more closely.
func getProcessMemoryBytes(processName string) (int64, error) {
	psName := strings.TrimSuffix(processName, ".exe")
	cmd := fmt.Sprintf(
		`$name='%s'; $escaped=[regex]::Escape($name); $samples=(Get-Counter '\Process(*)\Working Set - Private' -ErrorAction SilentlyContinue).CounterSamples | Where-Object { $_.InstanceName -match ('(?i)^' + $escaped + '(#\d+)?$') }; $sum=($samples | Measure-Object -Property CookedValue -Sum).Sum; if ($null -eq $sum) { 0 } else { [int64]$sum }`,
		psSingleQuote(psName),
	)
	out, err := hiddenCmd(
		"powershell", "-NoProfile", "-NonInteractive", "-Command", cmd,
	).CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("powershell failed: %s", strings.TrimSpace(string(out)))
	}
	value := strings.TrimSpace(string(out))
	bytes, parseErr := strconv.ParseInt(value, 10, 64)
	if parseErr != nil {
		return 0, fmt.Errorf("invalid memory value %q", value)
	}
	return bytes, nil
}

// isRunning reports whether any process with the given image name is active.
func isRunning(processName string) bool {
	out, _ := hiddenCmd(
		"tasklist",
		"/FI", fmt.Sprintf("IMAGENAME eq %s", processName),
		"/NH",
	).Output()
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(processName))
}

// kill forcefully terminates all instances of the named process.
func kill(processName string) error {
	out, err := hiddenCmd("taskkill", "/F", "/IM", processName).CombinedOutput()
	msg := strings.ToLower(string(out))
	// taskkill exits non-zero when the process isn't found — that's fine here.
	if err != nil && !strings.Contains(msg, "not found") && !strings.Contains(msg, "no tasks") {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// waitForExit polls until the process exits or the timeout elapses.
func waitForExit(processName string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isRunning(processName) {
			return true
		}
		time.Sleep(pollInterval)
	}
	return false
}

// waitForWindow runs a single hidden PowerShell process that polls internally
// until any instance of the named process has a visible main window, or until
// the timeout expires. One process, no flash loop.
func waitForWindow(processName string, timeout time.Duration) bool {
	psName := strings.TrimSuffix(processName, ".exe")
	secs := int(timeout.Seconds())
	psCmd := fmt.Sprintf(
		`$n=[regex]::Escape('%s'); $end=(Get-Date).AddSeconds(%d); `+
			`while ((Get-Date) -lt $end) { `+
			`if (Get-Process -EA SilentlyContinue | Where-Object { $_.ProcessName -match ('(?i)^'+$n) -and $_.MainWindowHandle -ne 0 }) { exit 0 }; `+
			`Start-Sleep -Milliseconds 500 }; exit 1`,
		psSingleQuote(psName), secs,
	)
	return hiddenCmd("powershell", "-NoProfile", "-NonInteractive", "-Command", psCmd).Run() == nil
}

// launch starts an executable detached so this utility can exit cleanly.
// This avoids cmd/start quoting edge-cases and handles spaces reliably.
func launch(exePath string) error {
	normalized := normalizeExePath(exePath)
	if normalized == "" {
		return fmt.Errorf("empty executable path")
	}
	if _, err := os.Stat(normalized); err != nil {
		return fmt.Errorf("executable not found at %q", normalized)
	}
	// U.GG is a GUI app that must show its own windows — do NOT use hiddenCmd here.
	cmd := exec.Command(normalized)
	return cmd.Start()
}

func defaultDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = "."
	}
	return filepath.Join(base, "ugg-memory-wrangler")
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

func loadMetrics(path string) (metrics, error) {
	var m metrics
	bytes, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, err
	}
	if err := json.Unmarshal(bytes, &m); err != nil {
		return metrics{}, err
	}
	return m, nil
}

func saveMetrics(path string, m metrics) error {
	bytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, bytes, 0o644)
}

func appendLog(path string, processName string, beforeBytes int64, afterBytes int64, freedBytes int64) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line := fmt.Sprintf(
		"%s\tprocess=%s\tbefore=%d\tafter=%d\tfreed=%d\n",
		time.Now().Format(time.RFC3339),
		processName,
		beforeBytes,
		afterBytes,
		freedBytes,
	)
	_, err = f.WriteString(line)
	return err
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KB", "MB", "GB", "TB"}
	return fmt.Sprintf("%.2f %s", float64(bytes)/float64(div), suffixes[exp])
}

func formatMemory(bytes int64) string {
	const mb = 1024 * 1024
	const gb = 1024 * mb

	if bytes < gb {
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(mb))
	}
	return fmt.Sprintf("%.2f GB", float64(bytes)/float64(gb))
}

func waitForEnter() {
	fmt.Println()
	fmt.Print("Press Enter to close... ")
	_, _ = fmt.Scanln()
}

func main() {
	rawArgs := os.Args[1:]
	autoPopup := len(rawArgs) == 0

	name := flag.String("name", defaultProcessName,
		"Image name of the process to kill/restart (e.g. ugg.exe)")
	path := flag.String("path", "",
		"Full path to the executable. Auto-detected from the running process if omitted.")
	delay := flag.Duration("delay", 2*time.Second,
		"How long to wait after killing before restarting (e.g. 2s, 500ms)")
	warmup := flag.Duration("warmup", 5*time.Second,
		"How long to wait after launch before measuring post-restart memory")
	dataDir := flag.String("data-dir", defaultDataDir(),
		"Directory for metrics state and logs")
	killOnly := flag.Bool("kill-only", false,
		"Terminate the process without restarting it")
	popup := flag.Bool("popup", false,
		"Show a Windows popup summary when finished")
	wait := flag.Bool("wait", false,
		"Wait for Enter before exiting (useful when double-clicking)")
	trackMemory := flag.Bool("track-memory", true,
		"Measure before/after memory and persist lifetime freed total")
	flag.Parse()

	showPopup := *popup || autoPopup
	waitOnExit := *wait && !showPopup

	// In GUI mode (double-click with no args): fully detach from the console
	// so no CMD window appears in the taskbar, then register the toast AUMID.
	if autoPopup {
		ensureToastAppID()
		syscall.NewLazyDLL("kernel32.dll").NewProc("FreeConsole").Call()
	}

	var progressFile string

	fail := func(format string, a ...any) {
		message := fmt.Sprintf(format, a...)
		fmt.Fprintln(os.Stderr, message)
		if progressFile != "" {
			writeProgress(progressFile, progressState{Status: "done", Message: message})
		} else if showPopup {
			notify("U.GG Restart Failed", []string{message}, true)
		}
		if waitOnExit {
			waitForEnter()
		}
		os.Exit(1)
	}

	// Console output helpers — no-op when running as GUI (double-click).
	out := func(s string) {
		if !autoPopup {
			fmt.Println(s)
		}
	}
	outf := func(format string, a ...any) {
		if !autoPopup {
			fmt.Printf(format, a...)
		}
	}

	exePath := *path
	beforeBytes := int64(0)
	afterBytes := int64(0)
	freedBytes := int64(0)
	lifetimeTotalBytes := int64(0)
	lifetimeRuns := 0

	metricsPath := filepath.Join(*dataDir, "metrics.json")
	logPath := filepath.Join(*dataDir, "runs.log")

	if *trackMemory {
		if err := ensureDir(*dataDir); err != nil {
			fail("failed to create data dir: %v", err)
		}
	}

	// Resolve the executable path from the live process before killing it.
	if exePath == "" && !*killOnly {
		detectedBy := ""
		exePath, detectedBy = resolveExePath(*name, *path)
		_ = detectedBy
		if exePath == "" {
			fail("Could not find U.GG on this system.\n\nOpen U.GG once and try again, or use -path to specify the executable location.")
		}
		_ = saveConfig(configData{ExePath: exePath})
		detectedName := strings.ToLower(filepath.Base(exePath))
		if *name == defaultProcessName && detectedName != "" && detectedName != strings.ToLower(*name) {
			*name = detectedName
		}
	}

	// In GUI mode: start the live window immediately after path is resolved,
	// before any long-running work begins.
	if autoPopup {
		progressFile = filepath.Join(os.TempDir(), "ugg-memory-wrangler-progress.json")
		writeProgress(progressFile, progressState{Status: "working"})
		showLiveWindow(progressFile, logPath)
	}

	var prog progressState
	prog.Status = "working"
	updateProgress := func() {
		if progressFile != "" {
			writeProgress(progressFile, prog)
		}
	}

	// Utility status header (CLI mode only)
	out("")
	out("  U.GG Memory Wrangler")
	out("  " + strings.Repeat("─", 36))

	if *trackMemory {
		if bytes, err := getProcessMemoryBytes(*name); err == nil {
			beforeBytes = bytes
			outf("  %-22s %s\n", "Before restart:", formatMemory(beforeBytes))
			prog.Before = formatMemory(beforeBytes)
			updateProgress()
		}
	}

	if isRunning(*name) {
		outf("  %-22s ", "Stopping U.GG...")
		if err := kill(*name); err != nil {
			fail("kill failed: %v", err)
		}
		if !waitForExit(*name, exitTimeout) {
			fail("timed out waiting for %s to exit", *name)
		}
		out("done")
		prog.Stopping = "done"
		updateProgress()
	} else {
		outf("  %-22s %s\n", "Status:", "U.GG was not running")
		prog.Stopping = "n/a"
		updateProgress()
	}

	if *killOnly {
		out("  " + strings.Repeat("─", 36))
		out("  U.GG has been stopped.")
		if showPopup {
			if autoPopup {
				prog.Message = "U.GG has been stopped."
				prog.Status = "done"
				updateProgress()
			} else {
				notify("U.GG Refresh Complete", []string{"U.GG has been stopped."}, false)
			}
		}
		if waitOnExit {
			waitForEnter()
		}
		return
	}

	outf("  %-22s ", "Starting U.GG...")
	time.Sleep(*delay)
	if err := launch(exePath); err != nil {
		fail("launch failed: %v", err)
	}
	// Wait until U.GG's window is actually visible on screen before reporting done.
	waitForWindow(*name, 30*time.Second)
	out("done")
	prog.Starting = "done"
	updateProgress()

	if !*trackMemory {
		out("  " + strings.Repeat("─", 36))
		out("  U.GG has been restarted.")
		if showPopup {
			if autoPopup {
				prog.Message = "U.GG has been restarted."
				prog.Status = "done"
				updateProgress()
			} else {
				notify("U.GG Refresh Complete", []string{"U.GG has been restarted successfully."}, false)
			}
		}
		if waitOnExit {
			waitForEnter()
		}
		return
	}

	outf("  %-22s ", "Letting U.GG settle...")
	time.Sleep(*warmup)
	out("done")
	prog.Warmup = "done"
	updateProgress()

	if bytes, err := getProcessMemoryBytes(*name); err == nil {
		afterBytes = bytes
		outf("  %-22s %s\n", "After restart:", formatMemory(afterBytes))
		prog.After = formatMemory(afterBytes)
		updateProgress()
		if beforeBytes > 0 {
			freedBytes = beforeBytes - afterBytes
		}
	}

	m, metricsErr := loadMetrics(metricsPath)
	if metricsErr != nil {
		if !autoPopup {
			fmt.Fprintf(os.Stderr, "  warning: could not read metrics, resetting: %v\n", metricsErr)
		}
		m = metrics{}
	}
	m.Runs++
	m.LastFreedBytes = freedBytes
	m.LastBeforeBytes = beforeBytes
	if freedBytes > 0 {
		m.TotalFreedBytes += freedBytes
	}
	m.UpdatedAt = time.Now().Format(time.RFC3339)
	if saveErr := saveMetrics(metricsPath, m); saveErr == nil {
		lifetimeTotalBytes = m.TotalFreedBytes
		lifetimeRuns = m.Runs
	}

	_ = appendLog(logPath, *name, beforeBytes, afterBytes, freedBytes)

	// Console summary (CLI only)
	out("  " + strings.Repeat("─", 36))
	switch {
	case beforeBytes == 0:
		outf("  %-22s %s\n", "Result:", "No baseline (first run)")
	case freedBytes > 0:
		outf("  %-22s %s\n", "Freed this run:", formatMemory(freedBytes))
	case freedBytes < 0:
		outf("  %-22s %s\n", "Memory increased by:", formatMemory(-freedBytes))
	default:
		outf("  %-22s %s\n", "Result:", "No change detected")
	}
	if lifetimeRuns > 0 {
		outf("  %-22s %s  (%d restart(s))\n", "Total freed:", formatMemory(lifetimeTotalBytes), lifetimeRuns)
		outf("  %-22s %s\n", "Avg. per restart:", formatMemory(lifetimeTotalBytes/int64(lifetimeRuns)))
	}
	out("  " + strings.Repeat("─", 36))

	// Notification / result window
	if showPopup {
		var notifLines []string
		switch {
		case beforeBytes == 0:
			prog.Freed = "N/A (first run)"
			notifLines = []string{"U.GG has been restarted.", "No memory baseline — U.GG was not running before restart."}
		case freedBytes > 0:
			prog.Freed = formatMemory(freedBytes)
			prog.FreedPos = true
			notifLines = []string{
				fmt.Sprintf("Freed %s this restart.", formatMemory(freedBytes)),
				fmt.Sprintf("Total freed: %s across %d restart(s).", formatMemory(lifetimeTotalBytes), lifetimeRuns),
			}
		case freedBytes < 0:
			prog.Freed = "+" + formatMemory(-freedBytes) + " (increased)"
			notifLines = []string{
				fmt.Sprintf("Memory increased by %s after restart.", formatMemory(-freedBytes)),
				fmt.Sprintf("Total freed: %s across %d restart(s).", formatMemory(lifetimeTotalBytes), lifetimeRuns),
			}
		default:
			prog.Freed = "No change detected"
			notifLines = []string{
				"U.GG restarted — no memory change detected.",
				fmt.Sprintf("Total freed: %s across %d restart(s).", formatMemory(lifetimeTotalBytes), lifetimeRuns),
			}
		}
		if lifetimeRuns > 0 {
			prog.Total = fmt.Sprintf("%s  (%d run(s))", formatMemory(lifetimeTotalBytes), lifetimeRuns)
			prog.Avg = formatMemory(lifetimeTotalBytes / int64(lifetimeRuns))
		}
		if autoPopup {
			// Signal the live window that work is complete.
			prog.Status = "done"
			updateProgress()
			// Also fire a toast so the result appears in Action Center.
			_ = showToast("U.GG Refresh Complete", notifLines)
		} else {
			notify("U.GG Refresh Complete", notifLines, false)
		}
	}

	if waitOnExit {
		waitForEnter()
	}
}
