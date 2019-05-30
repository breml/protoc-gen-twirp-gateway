package main

import (
	"fmt"
	"regexp"
	"text/template"

	pgs "github.com/lyft/protoc-gen-star"
	pgsgo "github.com/lyft/protoc-gen-star/lang/go"
	"google.golang.org/genproto/googleapis/api/annotations"
)

type GatewayModule struct {
	*pgs.ModuleBase
	ctx pgsgo.Context
	tpl *template.Template
}

func NewGatewayModule() *GatewayModule {
	return &GatewayModule{ModuleBase: &pgs.ModuleBase{}}
}

func (g *GatewayModule) InitContext(c pgs.BuildContext) {
	g.ModuleBase.InitContext(c)
	g.ctx = pgsgo.InitContext(c.Parameters())

	tpl := template.New("gw").Funcs(map[string]interface{}{
		"package":     g.ctx.PackageName,
		"name":        g.ctx.Name,
		"hasHttpRule": g.hasHttpRule,
		"method":      g.method,
		"pattern":     g.pattern,
		"handler":     g.handler,
	})

	g.tpl = template.Must(tpl.Parse(tmpl))
}

func (g *GatewayModule) Name() string {
	return "twirp-gateway"
}

func (g *GatewayModule) Execute(targets map[string]pgs.File, pkgs map[string]pgs.Package) []pgs.Artifact {
	for _, t := range targets {
		g.generate(t)
	}

	return g.Artifacts()
}

func (g *GatewayModule) generate(f pgs.File) {
	if len(f.Messages()) == 0 {
		return
	}

	name := g.ctx.OutputPath(f).SetExt(Extension)
	g.AddGeneratorTemplateFile(name.String(), g.tpl, f)
}

func (g *GatewayModule) hasHttpRule(m pgs.Method) bool {
	rule := &annotations.HttpRule{}
	ok, _ := m.Extension(annotations.E_Http, rule)
	return ok
}

func (g *GatewayModule) method(m pgs.Method) string {
	rule := &annotations.HttpRule{}
	m.Extension(annotations.E_Http, rule)

	switch verb := rule.Pattern.(type) {
	case *annotations.HttpRule_Get:
		return "GET"
	case *annotations.HttpRule_Put:
		return "PUT"
	case *annotations.HttpRule_Post:
		return "POST"
	case *annotations.HttpRule_Delete:
		return "DELETE"
	case *annotations.HttpRule_Patch:
		return "PATCH"
	case *annotations.HttpRule_Custom:
		return verb.Custom.Kind
	default:
		panic(fmt.Sprintf("google.api.HttpRule unexpected type %T", verb))
	}
}

var patternRE = regexp.MustCompile(`(?i)\{([a-z][a-z0-9_]+)\}`)

func (g *GatewayModule) pattern(m pgs.Method) string {
	rule := &annotations.HttpRule{}
	m.Extension(annotations.E_Http, rule)

	// TODO(shane): Compile the {name}, {name=message/*} stuff into a capture group.
	// "/hello/{name}"
	// "^/hello/(?P<name>[^/#?]+)$"
	//
	// I have no plans to support the {name=message/*} junk, it seems pretty whacky and Google specific.
	pattern := ""
	switch verb := rule.Pattern.(type) {
	case *annotations.HttpRule_Get:
		pattern = verb.Get
	case *annotations.HttpRule_Put:
		pattern = verb.Put
	case *annotations.HttpRule_Post:
		pattern = verb.Post
	case *annotations.HttpRule_Delete:
		pattern = verb.Delete
	case *annotations.HttpRule_Patch:
		pattern = verb.Patch
	case *annotations.HttpRule_Custom:
		pattern = verb.Custom.Path
	default:
		panic(fmt.Sprintf("google.api.HttpRule unexpected type %T", verb))
	}

	return "^" + patternRE.ReplaceAllString(pattern, `(?P<$1>[^/#?]+)`) + "[/#?]?$"
}

func (g *GatewayModule) handler(m pgs.Method) string {
	return "HANDLER"
}

// Extension for output.
const Extension = ".gw.go"

const tmpl = `
// Generated by protoc-gen-twirp-gateway. Do not edit.
package {{ package . }}

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type gatewayRoute struct {
	match    *regexp.Regexp
	endpoint string
}

func (g *gatewayRoute) try(path string) (url.Values, bool) {
	p := make(url.Values)
	matches := g.match.FindStringSubmatch(path)
	if matches != nil {
		for i, name := range g.match.SubexpNames() {
			if name != "" {
				p.Add(name, matches[i])
			}
		}
		return p, true
	}
	return nil, false
}

// Set arbitrary nested JSON with a value.
//
// Does not support integer paths only the nested dot notation.
//
// Opinion:
// * Google over engineered the hell out of this stuff.
// * JSON really needs an XML XPath style specification we can all agree on.
func gatewaySetJSON(data interface{}, path string, value interface{}) error {
	pp := strings.Split(path, ".")
	for _, p := range pp[:len(path)-1] {
		data = data.(map[string]interface{})[p] // TODO: Check cast.
	}

	// TODO: Check cast.
	data.(map[string]interface{})[pp[len(path)-1]] = value
	return nil
}

{{ range $index, $service := .Services }}
// {{ .Name }}Gateway middleware rewrites requests and merges query params into a Twerp RPC request.
func {{ .Name }}Gateway() func(next http.Handler) http.Handler {
	routes := make(map[string][]*gatewayRoute)

{{ range .Methods }}
{{ if hasHttpRule . }}
routes[{{ method . | printf "%q" }}] = append(routes[{{ method . | printf "%q" }}], &gatewayRoute{regexp.MustCompile({{ pattern . | printf "%q" }}), {{ $service.Name }}PathPrefix + {{ .Name | printf "%q" }}})
{{ end }}
{{ end }}

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			content := r.Header.Get("Content-Type")
			i := strings.Index(content, ";")
			if i == -1 {
				i = len(content)
			}

			// TODO(shane): Only JSON payloads supported.
			if strings.TrimSpace(strings.ToLower(content[:i])) != "application/json" {
				http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
				return
			}

			for _, gr := range routes[r.Method] {
				if params, ok := gr.try(r.URL.Path); ok {
					if len(params) > 0 {
						r.URL.RawQuery = url.Values(params).Encode() + "&" + r.URL.RawQuery
					}

					if len(r.URL.Query()) > 0 {
						// TODO(shane): Decode body, add query params to structure.
						// TODO(shane): What are the documented rules regarding body: "*".
						request := map[string]interface{}{}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							http.Error(w, err.Error(), http.StatusBadRequest)
							return
						}

						// TODO(shane): Only supports dot notation on string keys.
						query := r.URL.Query()
						for path := range query {
							// TODO(shane): Freak out on error!
							gatewaySetJSON(request, path, query.Get(path))
						}

						// TODO(shane): Freak out on error!
						encoded, _ := json.Marshal(request)

						body := bytes.NewReader(encoded)
						r.ContentLength = int64(body.Len())
						r.Body = ioutil.NopCloser(body)
					}

					r.Method = http.MethodPost
					r.URL.Path = gr.endpoint
					h.ServeHTTP(w, r)
					return
				}
			}

			allowed := make([]string, 0, len(routes))
			for method, handlers := range routes {
				if method == r.Method {
					continue
				}

				for _, gr := range handlers {
					if _, ok := gr.try(r.URL.Path); ok {
						allowed = append(allowed, method)
					}
				}
			}

			if len(allowed) == 0 {
				http.NotFound(w, r)
				return
			}

			w.Header().Add("Allow", strings.Join(allowed, ", "))
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		})
	}
}
{{ end }}
`
