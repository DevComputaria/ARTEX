// Package selfupdate implementa a atualização opcional por releases do GitHub.
// O pacote baixa, valida, testa e prepara o binário para substituição atômica no
// próximo início. Os scripts start.sh/start.bat apenas supervisionam o processo;
// validação, rollback e controle de tentativas permanecem em Go.
package selfupdate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Enabled informa se o mecanismo de atualização pela Internet foi habilitado.
// A distribuição corporativa opera sem contato automático com fornecedores:
// administrador precisa aceitar explicitamente esse tráfego com
// ARTEX_SELF_UPDATE=1.
func Enabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARTEX_SELF_UPDATE"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ExitRestart é o código de saída para o "Demonditor" para me puxar de volta.
// Executar novamente imediatamente, sem contar com uma retirada de crash. 0 indica que o usuário parou normalmente (ciclo de saídas do script), e o resto é considerado como colapso.
const ExitRestart = 75

// O maxAttempts é o número de tentativas de inicialização permitidas após recarregar. Uma nova versão conta +1 cada início e sobrevive.
// settleDelay limpa as tags; o maxAttempts que está falhando significa que a nova edição não está chegando e volta automaticamente.
const maxAttempts = 3

// Paths é o documento inteiro envolvido na atualização única e está centralmente ligado ao diretório** do executável.
// CWD: O diretório de execução pode ser / ou qualquer caminho, usando CWD para trazer a memória temporária para o
// Em outro lugar, a lógica de recarregamento é diretamente inválida.
type Paths struct {
	Dir     string // Pasta de Ficheiros Executáveis
	Current string // Binário em execução atual        artex      / artex.exe
	New     string // Gravada a nova versão            artex.new  / artex.new.exe
	Sum     string // SHA256 da nova versão (hex) artex.new.sha256 / artex.new.exe.sha256
	Old     string // Alterar versão antiga do backup antes de carregar      artex.old  / artex.old.exe
	Marker  string // Marcador de Estado de Atualização            artex.upgrade.json
}

// O ResolvePaths direciona todos os caminhos de atualização para o executável atual.
//
// Windows em .new/.old também deve ser acompanhado por sufixo .exe, caso contrário, o teste de fumaça e a substituição falharão.
// Então, removemos o sufixo e soletremos, e as duas plataformas são chamadas simétricamente.
func ResolvePaths() (Paths, error) {
	exe, err := os.Executable()
	if err != nil {
		return Paths{}, fmt.Errorf("localizar executável: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	name := filepath.Base(exe)
	ext := filepath.Ext(name) // ".exe" no Windows; normalmente vazio no Unix.
	stem := strings.TrimSuffix(name, ext)

	join := func(suffix string) string { return filepath.Join(dir, stem+suffix+ext) }
	return Paths{
		Dir:     dir,
		Current: exe,
		New:     join(".new"),
		Sum:     join(".new") + ".sha256",
		Old:     join(".old"),
		Marker:  filepath.Join(dir, stem+".upgrade.json"),
	}, nil
}

// marker registra o progresso de uma operação de recarga e é usado para ativar o rollback automático quando a nova edição não aparece.
type marker struct {
	From     string `json:"from"`     // Versão pré-actualização
	To       string `json:"to"`       // Versão do Alvo
	Attempts int    `json:"attempts"` // Número de tempos de inicialização tentados após recarregamento
	StagedAt int64  `json:"staged_at"`
}

func readMarker(path string) (marker, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return marker{}, false
	}
	var m marker
	if json.Unmarshal(b, &m) != nil {
		return marker{}, false
	}
	return m, true
}

func writeMarker(path string, m marker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// O cleanStaged limpa a memória temporária. A recarga é bem- sucedida, a verificação falha, o usuário a cancelou e as restantes são evitadas.
// artex.new está tentando novamente na próxima inicialização.
func cleanStaged(p Paths) {
	_ = os.Remove(p.New)
	_ = os.Remove(p.Sum)
}

// CompareVersions compara versões e retorna -1, 0 ou 1.
// ok=false indica que pelo menos um lado não é um número de versão comparável (p. ex. "dev" ou
// git describe saída "0.3.7-2-gabc1234-dirty" em que ponto o chamador deve desativar uma atualização chave,
// Caso contrário, a construção em desenvolvimento "aumentada" seria oficializada e cobriria as mudanças não apresentadas.
func CompareVersions(a, b string) (int, bool) {
	av, aok := parseVersion(a)
	bv, bok := parseVersion(b)
	if !aok || !bok {
		return 0, false
	}
	for i := range 3 {
		if av[i] != bv[i] {
			if av[i] < bv[i] {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// parseVersion resolve a versão de "v0.3.7"/ "0.3.7" com o número de [3] int.
//
// Apenas versões puras com três segmentos são aceitas.
// "0.3.7-2-gabc1234" versões deste tipo de sufixo devem ser consideradas não-comparativas em vez de tratadas como
// 0.3.7 - Caso contrário, a construção do desenvolvimento será mal calculada como "mais recente" ou coberta pela versão oficial.
func parseVersion(s string) ([3]int, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return [3]int{}, false
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return [3]int{}, false
	}
	var out [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		out[i] = n
	}
	return out, true
}

// Se o processo de relatório InDocker está em execução no recipiente. O sub- carregamento Docker está escrito na camada de gravação da embalagem.
// `docker compose up -d` Reconstruindo o recipiente retorna a versão do próprio espelho -- este é o comportamento esperado.
// (O usuário já estava tentando re-cenar a imagem), mas a parte frontal precisa ser clara sobre isso.
func InDocker() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	b, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	s := string(b)
	return strings.Contains(s, "docker") || strings.Contains(s, "containerd")
}
