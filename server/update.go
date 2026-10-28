package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/Autumn-27/artex/selfupdate"
)

// O lado HTTP actualizado pela primeira chave da página. A lógica real de transferência/check/place está no pacote selfupdate.
// É apenas sobre a fronteira do poder, e a cross-bid, a transmissão do progresso, e o "deve sair" para main.
//
// O relançamento não é feito por este processo: salve uma nova versão do processo e saia com selfupdate.ExitRestart.
// Re-iniciado pelo script Guardian (start.sh/start.bat, Docker, ENTRYPOINT).

// restartCh fecha após atualização ou rollback está completo, main sai com ExitRestart após receber.
var (
	restartOnce sync.Once
	restartCh   = make(chan struct{})
)

// RestartRequested retorna um channel fechado em "Por favor, saia e deixe o servidor me puxar de volta".
func RestartRequested() <-chan struct{} { return restartCh }

func requestRestart() { restartOnce.Do(func() { close(restartCh) }) }

// bootState é a conclusão do selfupdate.Bootstrap nesta inicialização (reboque com sucesso/ apenas rebole/
// O depósito temporário foi descartado) e injetado pelo main com o propósito de dizer ao lado da frente exatamente o que aconteceu com a última atualização.
var (
	bootStateMu sync.Mutex
	bootState   selfupdate.State
)

// SetBootUpdateState é chamado uma vez por main na inicialização.
func SetBootUpdateState(st selfupdate.State) {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	bootState = st
}

func bootUpdateState() selfupdate.State {
	bootStateMu.Lock()
	defer bootStateMu.Unlock()
	return bootState
}

// Cache releaseCache O resultado da última consulta de versão do GitHub.
//
// A dica "com uma nova versão" na barra superior é verificada uma vez que uma página é carregada, enquanto o GitHub API não certificado é
// Cada IP 60 vezes por hora -- se você não diminuir, você fica sem quotas abrindo mais algumas abas ou escovando páginas.
// Você não pode verificar se deseja atualizar. Você pode ignorar a cache com force quando você tiver um ponto visível de "verificar as atualizações".
type releaseCache struct {
	mu  sync.Mutex
	rel *selfupdate.Release
	err error
	at  time.Time
	// fetch é a função de numeração, que é o ponto de injeção deixado para o teste; a verdadeira consulta GitHub é feita para nil.
	fetch func(context.Context, *http.Client) (*selfupdate.Release, error)
}

const (
	releaseTTL = 30 * time.Minute
	// A falha também diminui por um curto período de tempo, caso contrário GitHub é para secar cada carregamento de página quando não é possível alcançá-lo e esperar o tempo fora;
	// Mas TTL é curto, e a rede estará de volta por conta própria em breve.
	releaseErrTTL = 2 * time.Minute
	// O tempo limite para a consulta. O tempo limite de 30 minutos para o NewClient é para baixar o pacote como um todo, e a versão de verificação não pode esperar tanto tempo.
	releaseTimeout = 20 * time.Second
)

var relCache = &releaseCache{}

// get retorna o mais recente Release, enquanto o cache de sucesso não acessa a rede.
//
// Sempre segurando bloqueios durante a contagem: a solicitação é na fila para o mesmo resultado, em vez de jogar GitHub cada
// (A página é tão fácil quanto a página é limitada quando várias abas são verificadas simultaneamente.
func (c *releaseCache) get(ctx context.Context, client *http.Client, force bool) (*selfupdate.Release, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !force {
		ttl := releaseTTL
		if c.err != nil {
			ttl = releaseErrTTL
		}
		if !c.at.IsZero() && time.Since(c.at) < ttl {
			return c.rel, c.err
		}
	}

	fetch := c.fetch
	if fetch == nil {
		fetch = selfupdate.FetchLatest
	}
	ctx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	rel, err := fetch(ctx, client)
	// A solicitação foi cancelada (a página do usuário desligada) não significa que o GitHub tenha um problema. Não escreva na cache.
	// Caso contrário, o próximo visitante terá um estranho erro "cancelado".
	if err != nil && ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return c.rel, err
	}
	c.rel, c.err, c.at = rel, err, time.Now()
	return rel, err
}

// updateProgress é um progresso para a frente.
type updateProgress struct {
	Phase   selfupdate.Phase `json:"phase"`
	Percent int              `json:"percent"` // Percentual válido somente durante o download; -1 nos demais estados.
	Message string           `json:"message"`
	Version string           `json:"version,omitempty"`
	Error   string           `json:"error,omitempty"`
}

// updateHub mantém o progresso para uma atualização e transmissões para assinantes SSE.
//
// running impede duas goroutines de escrever no mesmo artex.new.
type updateHub struct {
	mu      sync.Mutex
	running bool
	cur     updateProgress
	subs    map[chan updateProgress]struct{}
}

var updHub = &updateHub{
	cur:  updateProgress{Phase: selfupdate.PhaseIdle, Percent: -1},
	subs: map[chan updateProgress]struct{}{},
}

// begin tomou a permissão de atualização e está em andamento retornando false.
func (h *updateHub) begin(version string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return false
	}
	h.running = true
	h.cur = updateProgress{Phase: selfupdate.PhaseDownload, Percent: 0, Message: "Pronto.…", Version: version}
	h.fanout(h.cur)
	return true
}

// finish termina uma atualização. err indica que o armazenamento temporário foi bem sucedido para nil, aguardando o reinício.
func (h *updateHub) finish(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.running = false
	if err != nil {
		h.cur = updateProgress{Phase: selfupdate.PhaseFailed, Percent: -1, Message: "A actualização falhou", Error: err.Error(), Version: h.cur.Version}
	} else {
		h.cur = updateProgress{Phase: selfupdate.PhaseStaged, Percent: 100, Message: "Nova versão pronta; reiniciando…", Version: h.cur.Version}
	}
	h.fanout(h.cur)
}

func (h *updateHub) publish(ph selfupdate.Phase, pct int, msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cur = updateProgress{Phase: ph, Percent: pct, Message: msg, Version: h.cur.Version}
	h.fanout(h.cur)
}

// fanout deve ser chamado quando segurando h.mu. O assinante channel é tamponado e então jogado fora -
// O progresso é uma informação instantânea perdida e não deve ser permitido bloquear uma conexão SSE presa para atualizar-se.
func (h *updateHub) fanout(p updateProgress) {
	for ch := range h.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

func (h *updateHub) snapshot() (updateProgress, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cur, h.running
}

func (h *updateHub) subscribe() (<-chan updateProgress, func()) {
	ch := make(chan updateProgress, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
			close(ch)
		})
	}
}

// updateCheck consulta a versão oficial mais recente no GitHub e compará-la com a versão atual.
//
// api.github.com (CORS para ZXXZ1QXZ é *) na extremidade dianteira, mas** é baseado nesta interface**:
// Os downloads são feitos na parte de trás, e só com acesso ao GitHub você pode atualizá-los. Navegadores podem se conectar aos servidores.
// É comum (os servidores só estão disponíveis na Intranet ou no navegador) e é inevitável que as atualizações falhem.
// Seria melhor comunicar o erro como está nesta fase do exame.
func (s *Server) updateCheck(w http.ResponseWriter, r *http.Request) {
	current := BuildVersion
	mode := "binary"
	if selfupdate.InDocker() {
		mode = "docker"
	}
	boot := bootUpdateState()
	out := map[string]any{
		"current":     current,
		"mode":        mode,
		"os":          runtime.GOOS,
		"arch":        runtime.GOARCH,
		"has_backup":  selfupdate.HasBackup(),
		"repo":        selfupdate.Repo,
		"boot_notice": boot.Detail,
		"rolled_back": boot.RolledBack,
	}
	if !selfupdate.Enabled() {
		out["disabled"] = true
		out["reason"] = "A atualização pela Internet está desativada neste perfil local. Defina ARTEX_SELF_UPDATE=1 para habilitá-la."
		writeJSON(w, 200, out)
		return
	}

	// Topbar dica para andar cache (padrão); force=1 Forçado retornar fonte quando o ponto de usuário "verificar para atualização".
	force := r.URL.Query().Get("force") != ""
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, force)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, 200, out)
		return
	}

	latest := rel.TagName
	out["latest"] = latest
	out["notes"] = rel.Body
	out["html_url"] = rel.HTMLURL
	if !rel.PublishedAt.IsZero() {
		out["published_at"] = rel.PublishedAt.Format(time.RFC3339)
	}

	asset := selfupdate.AssetName(latest, runtime.GOOS, runtime.GOARCH)
	out["asset"] = asset
	if a, ok := rel.FindAsset(asset); ok {
		out["asset_available"] = true
		out["size"] = a.Size
	} else {
		out["asset_available"] = false
	}

	cmp, comparable := selfupdate.CompareVersions(current, latest)
	out["comparable"] = comparable
	out["has_update"] = comparable && cmp < 0
	if !comparable {
		// Desenvolver uma construção (dev / git describe com um sufixo) sem número de versão comparável.
		// Cobre binários de depuração locais com uma versão oficial, portanto eles não são atualizados diretamente.
		out["reason"] = fmt.Sprintf("a versão atual %q não é uma release oficial; atualização desativada", current)
	}
	writeJSON(w, 200, out)
}

// updateApply baixa e salva uma nova versão, após o que o processo sai para o script Guardian reinicia.
//
// Voltar para 202, trabalhar no backstage goroutine: O download do pacote pode demorar alguns minutos, mas o download não é uma boa idéia.
// A operação retorna 202 imediatamente; o progresso é enviado por /api/update/stream.
func (s *Server) updateApply(w http.ResponseWriter, r *http.Request) {
	if !selfupdate.Enabled() {
		writeErr(w, http.StatusForbidden, "atualização pela Internet desativada; defina ARTEX_SELF_UPDATE=1 para habilitar")
		return
	}
	current := BuildVersion

	// Cache: Certifique-se de que está carregado na versão que o usuário viu e confirmou na interface.
	client := selfupdate.NewClient(s.m.GlobalProxy())
	rel, err := relCache.get(r.Context(), client, false)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	cmp, comparable := selfupdate.CompareVersions(current, rel.TagName)
	if !comparable {
		writeErr(w, 400, fmt.Sprintf("a versão atual %q não é uma release oficial; atualização desativada", current))
		return
	}
	if cmp >= 0 {
		writeErr(w, 400, fmt.Sprintf("É a última versão. %s", current))
		return
	}
	if !updHub.begin(rel.TagName) {
		writeErr(w, 409, "Está em curso uma actualização")
		return
	}

	go func() {
		// A resposta à solicitação de retorno terminou com o uso deliberado do s.ctx em vez do ctx solicitado: HTTP, que foi a primeira vez que o pedido de retorno foi devolvido.
		// Os downloads que se apegam a ele serão cancelados imediatamente.
		err := selfupdate.Stage(s.ctx, client, rel, current, func(ph selfupdate.Phase, pct int, msg string) {
			updHub.publish(ph, pct, msg)
		})
		updHub.finish(err)
		if err != nil {
			log.Printf("[update] A actualização falhou：%v", err)
			return
		}
		log.Printf("[update] %s → %s preparado; encerrando para concluir a troca", current, rel.TagName)
		// Deixa um pouco de tempo para empurrar o último progresso para a frente e aciona a saída.
		time.Sleep(1500 * time.Millisecond)
		requestRestart()
	}()

	writeJSON(w, 202, map[string]any{"ok": true, "target": rel.TagName})
}

// updateRollback restaura a versão anterior preservada antes da atualização.
func (s *Server) updateRollback(w http.ResponseWriter, r *http.Request) {
	if !selfupdate.Enabled() {
		writeErr(w, http.StatusForbidden, "atualização automática desativada neste perfil local")
		return
	}
	if _, running := updHub.snapshot(); running {
		writeErr(w, 409, "há uma atualização em andamento; rollback indisponível")
		return
	}
	if err := selfupdate.Rollback(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	log.Printf("[update] rollback manual concluído; encerrando para aplicar a versão anterior")
	writeJSON(w, 202, map[string]any{"ok": true})
	go func() {
		time.Sleep(500 * time.Millisecond)
		requestRestart()
	}()
}

// updateStream para exportar o progresso da atualização.
func (s *Server) updateStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch, unsub := updHub.subscribe()
	defer unsub()

	send := func(p updateProgress) {
		b, _ := json.Marshal(p)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}
	// Adicione um status atual e você pode ver a atualização em andamento imediatamente após a página ser atualizada.
	cur, _ := updHub.snapshot()
	send(cur)

	ctx := r.Context()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				return
			}
			send(p)
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
