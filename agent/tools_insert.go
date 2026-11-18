package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates Extracts a domain/IP/URL candidate string to insert into the asset input
// for asset interception matching. The host of the URL is declassified so that service/endpoint
// assets with "URL only" are also hit by the domain/IP rule.
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel Returns a short identifier of the asset to be inserted, used to intercept
// description messages.
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(unknown)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"A batch register of new finding assets, which may mix multiple types at one time (type see list). \n"+
			"Required fields for each type are: root_domain→domain; ip→ip (must be IPv4/IPv6, not hostname); subdomain→ domain; app→ app_name; service(HTTP)→url; service (not HTTP) → service_name+port (at least one ip/domain must be filled in); endpoint→url+method. The rest of the fields have their own meaning. \n"+
			"auth/technologies/params Append, not overwrite. \n"+
			"Returns: {results:[{index,id,type}], errors:[ {index,error}]}",
		obj(map[string]any{
			// task_id is not exposed to the model: which task the worker belongs to is assigned by
			// the program through SetTaskID (see handler).
			"assets": map[string]any{
				"type":        "array",
				"description": "Asset array, each element corresponds to an asset log",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "Set Type",
					},
					// root_domain / subdomain
					"domain":      str("Root domain name or subdomain (root_domain/subdomain must be filled)"),
					"icp":         str("ICP filing number (optional)"),
					"record_type": str("DNS Parsing type: A/AAAA/CNAME/MX etc. (subdomain optional)"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS List of parse values (subdomain optional, e.g. [\"1.2.3.4\",\"2.24.5\"])",
					},
					// ip
					"ip": str("IP address, must be a IPv4/IPv6 address, can not be a hostname (use the domain field of type=subdomain for the hostname); ip type is required; service/endpoint type is available, used to associate IP"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "List of domain names associated with this IP (optional for the ip type)",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "Open Port List (ip type optional)",
						"items": obj(map[string]any{
							"port":    intp("Port number"),
							"service": str("Service name, e. g. http/ssh/mysql (optional)"),
						}, "port"),
					},
					// app
					"app_name":    str("Apply name (app type to fill)"),
					"bundle_id":   str("Bundle ID (app type optional)"),
					"category":    str("Apply classification (optional)"),
					"description": str("Apply description (optional)"),
					"app_icp":     str("Apply ICP filing (optional)"),
					"company_id":  intp("id belonging to the enterprise (app type optional; p can't be attributed automatically by scope and needs to be clearly specified. id returns from add_company_scope)"),
					// service (http)
					"url":         str("Complete URL, including protocols and ports (HTTP service required; service_type automatically set to http)"),
					"status_code": intp("HTTP Response state code, e.g. 200/301/403/404 (optional)"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP Response bytes (optional)",
					},
					"page_title":   str("Page <title> (optional)"),
					"favicon_mmh3": str("Favicon MMH3 Hashi (optional)"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "List of fingerprint/technological stacks, e.g. [\"Nginx\",\"Vue\", \"Bootstrap\"] (optional)",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "A list of authentication information for finding, each containing fields such as type/username/password (optional, additional is not overridden)",
						"items":       map[string]any{"type": "object"},
					},
					// service (other, non-HTTP)
					"service_name": str("Service name, e.g. ssh/mysql/redis (required if service is not HTTP)"),
					"port":         intp("Port number (service not HTTP filled)"),
					// endpoint
					"method": str("HTTP Method: GET/POST/PUT/PATCH/DELETE, etc. (endpoint required)"),
					"params": map[string]any{
						"type":        "array",
						"description": "List of request parameters, each containing location(query/body/header/path)/name/value/type (optional, additional not covered)",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets Not enabled: AssetStore not initialized"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id is assigned by the program authoritatively (worker: SetTaskID), and does not
			// accept model input – avoiding model leakage/error transmission leading to assets not
			// being assigned to tasks or being assigned to wrong tasks.
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// The asset gateway rules are loaded once; read failures skip the decision (do not
			// block insertion).
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// Asset gateway: first blocked, then allowed, denied assets are not inserted
				// (skipping upsert and subsequent side effects).
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("Asset %s %s, prohibited insertion", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent through insert_assets"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker intent #%d Pass insert_assets Register", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// Auto-in test scope (source='auto'): only the worker top level explicitly inserted
				// this item, conservatively expands the scope by its type; side-effect derived
				// assets do not pass through here, so the scope is not blindly expanded. No action
				// when taskID=0. Not related to the coverage switch: task_scope is the scope
				// boundary of the task (the filter baseline for the list/query), the coverage
				// switch only decides whether to use it as a denominator deduction indicator, not
				// whether to cumulate the scope itself.
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"Adding a domain name/IP/CIDR/ICP file/business keyword to a company's [asset range] - domain names, networks and ICP will automatically take the assigned enset, and the keyword is provided only to Agent as a scoping hint. \n"+
			"The name of the company is only: company does not exist or is created, and re-uses it if it already exists (only the range is included). \n"+
			"Scope line, automatic system recognition: Root domain name / URL / Single IP / CIDR section / ICC file / Enterprise keyword. \n"+
			"The reason for attribution (whois/Certificate/ASN, etc.) must be explained to reason. \n"+
			"Bar: Deny naked TLD and hypernetic (IPv4 prefix is/16-/32, IPv6 prefix / 32-/128) and the illegal movement will be skipped and returned in errors.",
		obj(map[string]any{
			"company": str("Name of company (new if not available, new if existing; name only)"),
			"scope":   str("asset range, line by line: domain name / URL / IP / CIDR / ICP registration / enterprise keywords"),
			"reason":  str("Basis of attribution (evidence/source), to be completed"),
			"logo":    str("Company icon URL (optional; effective only on start-up)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope Not enabled: CompanyStore not initialized"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("Company can't be empty."), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("Failed to create/get a company: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"Add [this task] to the test scope – this is the authorization boundary of this task and the denominator of the asset testing coverage. \n"+
			"kind supports: company (entire company name asset) / root_domain (whole root domain, including all subdomains) / subdomain (single exact subdomain) / ip / cidr / icp / keyword."+
			"Note: Worker meets the main opportunity on a case-by-case basis and is added to the system [automatically] (precision subdomains); this tool is used for [actively expanding] - to include the whole root domain/business as a whole, or to supplement the designation of a subarea/IP. \n"+
			"value: company transmits company name or id (the company must already exist); root_domain/subdomain transmits domain name; ip/cidr transfers IP address or segment; icp/keyword transmitt registration number or enterprise keyword. \n"+
			"Please explain the basis (auditable) to reason. Multiple entries are used in arrays.",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "Batch: [{kind, value}].kind∈company/root_domain/subdomain/ip/cidr/icp/keyword.", "items": map[string]any{"type": "object"}},
			"kind":    str("[single] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[Single thread] Company name or id / Domain name / IP / CIDR / ICP / Keywords"),
			"reason":  str("Add basis (for audit) to be completed"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope Not enabled: AssetStore not initialized"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope Requires task context (no task at present)"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // Single-bar mode
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"Queries an asset within [Bentask and direct links], which is not covered by the factual anchor (relations only read for your own judgement as to whether or not to supplement or not substitute for your decision-making). \n"+
			"Optional filtering by asset type: root_domain/subdomain/service/app/endpoint/ip. \n"+
			"Page: page from 1, page_size default 10. Returns {assets:[{id,type,label}], total, page, page_size}.",
		obj(map[string]any{
			"type":      str("asset type filter (optional): root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("Page number from 1 (default 1)"),
			"page_size": intp("Number of pages per page (default 10)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets Not enabled: AssetStore not initialized"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets Need a task context"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"Query theasset library: a DSL expression search, or press id/ids; support page break. Only return to an asset within the range of the test [this task and direct associationtask]. \n"+
			"DSL: field=value ambiguous (ILIKE) | field==values accurate | field!=valued excluded | numeric field support > >= < <= | blank word=full text ambiguity; AND/OR combinations (AND high priority), can be grouped with brackets. asset types have separate type parameters, not written into DSL. \n"+
			"dsl when not sent id/ids must be non-empty (no unconditional full-volume search is allowed). \n"+
			"Available fields: domain (root/sub/service domain name), root_domain, ip, url, page_title, icp, service_name, app_name, method (e.g. GET/POST) service_type (http|other), record_type (e,g., A/CNAME), technology (array, = vague == accurate), port/status_code/company_id (integer). \n"+
			"Example: status_code>=400 AND technology=shiro ; (port==80 OR port==443) AND technology=\"nginx\"",
		obj(map[string]any{
			"dsl":    str(`DSL query expression (syntax/field see tool description). When not sent id/ids must be non-empty.`),
			"type":   str("Asset type filtering: root_domain|ip|subdomain|app|service|endpoint (independent field, can be overlaid with dsl; single type is not sufficient for query, still requires dsr)"),
			"id":     intp("Take it directly on a single set id (optional, with dsl/type each)"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Take directly to multiple sets id (optional, with dsl/type"},
			"limit":  intp("returns ceiling, decault 10 (optional)"),
			"offset": intp("Page offset, default 0 (optional)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets Not enabled: AssetStore not initialized"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("dsl cannot be empty when id/ids is not passed: unconditional query of all assets is not allowed, please provide the query conditions"), nil
			}
			if err != nil {
				return actool.Errorf("DSL error: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"Lists the [company/company] in the asset library and its scope with the number of attributed assets."+
			"Get company_id (insert_assets associated app, list_assets used when filtering by company_id)."+
			"Selectable search filters (without case-sensitive) by the name of the company, leaving empty to return all.",
		obj(map[string]any{
			"search": str("Filtering vaguely by corporate name (optional, case-sensitive); leaving empty to return all"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies Not enabled: CompanyStore not initialized"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("_Other Organiser " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings Reserve: Check for vulnerabilities identified by this task before reporting
		// a vulnerability to avoid repeatedly reporting the same vulnerability.
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope Not given to worker: defines the scope of enterprise assets under the
		// responsibility of the planning/controlling/auto role, the worker only performs
		// exploration.
		t.insertAssets(), t.listAssets(),
		// Cross work recall: Worker can also reuse observations from other work to avoid
		// duplication of work. search_all_worker_traces: Search all worker traces by keyword
		// without knowing intent_id; get_worker_trace: Lock the following steps of a work/locally
		// search/get the entire contents.
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail: The worker gets the intent_id/node id to check for the complete details of
		// the node (in conjunction with the review above).
		t.nodeDetail(),
		// The following tools are still [not given] to worker, and are left for planner/main (the
		// context-read, cross-work replication is the planning role, and worker only executes and
		// writes back single intents): list_facts / list_companies / list_worker_traces.
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: A person can inject corrective instructions (without interrupting, without
		// losing progress) to a running intent (work) in real time.
		t.steerWorkTool(),
		// set_goals: A new end goal can be added to the task during execution (the planner will
		// re-judge whether it was achieved).
		t.setGoals(),
		// set_constraints: Allow/deny restrictions on the task during execution, limiting the
		// exploration boundaries of the planner/worker.
		t.setConstraints(),
		// asset management (handlers guard nil store internally)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: checks untested assets within the scope of this mandate (type +
		// page) as required, and retests at its discretion.
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
