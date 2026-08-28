package winservice

import (
	"fmt"
	"os/exec"
	"strings"
)

// firewallRuleName — фиксированное DisplayName правила, по которому мы его
// находим/создаём/удаляем. Не зависит от текущего порта в конфиге — если
// порт сменили, правило с этим именем просто не совпадёт по LocalPort;
// FirewallRuleExists это не проверяет (см. её комментарий).
const firewallRuleName = "multirtmp API"

// Правила проверяются/создаются/удаляются через модуль PowerShell
// NetSecurity (Get-/New-/Remove-NetFirewallRule), а не через
// `netsh advfirewall firewall show rule` — у netsh сообщение "правило не
// найдено" локализовано под язык Windows (на русской системе — по-русски),
// парсить его текстом ненадёжно. `[bool]`-результат PowerShell от наличия
// объекта не зависит от локали никак.
func runPowerShell(script string) (string, error) {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// FirewallRuleExists сообщает, существует ли правило firewallRuleName —
// независимо от того, на какой порт оно заведено (проверять совпадение
// порта себе дороже: разные версии/среды по-разному отдают LocalPort в
// текстовом виде). Практический эффект отличия невелик: если порт сменили
// уже после того, как правило один раз создали под старый, эта проверка
// решит, что всё в порядке, хотя новый порт не открыт. Достаточно для
// текущей задачи — "рабочего правила нет вообще" остаётся надёжно поймано.
func FirewallRuleExists() (bool, error) {
	out, err := runPowerShell(fmt.Sprintf(
		`[bool](Get-NetFirewallRule -DisplayName '%s' -ErrorAction SilentlyContinue)`, firewallRuleName))
	if err != nil {
		return false, fmt.Errorf("winservice: failed to query firewall rule: %w (%s)", err, out)
	}
	return strings.EqualFold(out, "True"), nil
}

// AddFirewallRule открывает port для входящих TCP-соединений в приватных
// сетях (profile=Private — не публичных/доменных, см. CLAUDE.md/задание:
// "только частные сети").
func AddFirewallRule(port string) error {
	script := fmt.Sprintf(
		`New-NetFirewallRule -DisplayName '%s' -Direction Inbound -Action Allow -Protocol TCP -LocalPort %s -Profile Private | Out-Null`,
		firewallRuleName, port)

	if out, err := runPowerShell(script); err != nil {
		return fmt.Errorf("winservice: failed to add firewall rule: %w (%s)", err, out)
	}

	return nil
}

// RemoveFirewallRule удаляет правило firewallRuleName, если оно есть —
// Uninstall всегда может звать эту функцию, даже если правило никогда не
// создавалось (addr был loopback всё время).
//
// Явная проверка Get-NetFirewallRule перед Remove — не украшательство:
// Remove-NetFirewallRule на несуществующем правиле, даже под
// -ErrorAction SilentlyContinue, всё равно оставляет процесс powershell.exe
// с ненулевым кодом возврата (сама ошибка молчит, но её след остаётся в
// $Error и просачивается в exit code) — проверено живьём на этой машине.
// Раз уже проверяем наличие, отдельный `; exit 0` не нужен: если правила
// нет, Remove вообще не вызывается.
func RemoveFirewallRule() error {
	script := fmt.Sprintf(
		`if (Get-NetFirewallRule -DisplayName '%s' -ErrorAction SilentlyContinue) { Remove-NetFirewallRule -DisplayName '%s' -ErrorAction Stop }`,
		firewallRuleName, firewallRuleName)

	if out, err := runPowerShell(script); err != nil {
		return fmt.Errorf("winservice: failed to remove firewall rule: %w (%s)", err, out)
	}

	return nil
}
