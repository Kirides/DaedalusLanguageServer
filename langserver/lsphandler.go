package langserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.lsp.dev/jsonrpc2"
	lsp "go.lsp.dev/protocol"
	"go.lsp.dev/uri"
)

// LspHandler ...
type LspHandler struct {
	TextDocumentSyncHandler jsonrpc2.Handler
	bufferManager           *BufferManager
	parsedDocuments         *parseResultsManager
	initialDiagnostics      map[string][]lsp.Diagnostic
	baseLspHandler
	initialized bool
}

var (
	// ErrWalkAbort should be returned if a walk function should abort early
	ErrWalkAbort = fmt.Errorf("OK")
)

// NewLspHandler ...
func NewLspHandler() *LspHandler {
	bufferManager := NewBufferManager()
	parsedDocuments := newParseResultsManager()
	logLv := LogLevelInfo
	return &LspHandler{
		baseLspHandler: baseLspHandler{
			LogLevel: logLv,
		},
		initialized:     false,
		bufferManager:   bufferManager,
		parsedDocuments: parsedDocuments,
		TextDocumentSyncHandler: &textDocumentSyncHandler{
			baseLspHandler: baseLspHandler{
				LogLevel: logLv,
			},
			bufferManager:   bufferManager,
			parsedDocuments: parsedDocuments,
		},
	}
}

func completionItemFromSymbol(s Symbol) (lsp.CompletionItem, error) {
	kind, err := completionItemKindForSymbol(s)
	if err != nil {
		return lsp.CompletionItem{}, err
	}
	return lsp.CompletionItem{
		Kind:   kind,
		Label:  s.Name(),
		Detail: s.String(),
		Documentation: lsp.MarkupContent{
			Kind:  lsp.PlainText,
			Value: s.Documentation(),
		},
	}, nil
}

func completionItemKindForSymbol(s Symbol) (lsp.CompletionItemKind, error) {
	switch s.(type) {
	case VariableSymbol:
		return lsp.VariableCompletion, nil
	case ConstantSymbol:
		return lsp.ConstantCompletion, nil
	case FunctionSymbol:
		return lsp.FunctionCompletion, nil
	case ClassSymbol:
		return lsp.ClassCompletion, nil
	case ProtoTypeOrInstanceSymbol:
		return lsp.ClassCompletion, nil
	}
	return lsp.CompletionItemKind(-1), fmt.Errorf("Symbol not found")
}

func (h *LspHandler) handleTextDocumentCompletion(ctx context.Context, params *lsp.CompletionParams) ([]lsp.CompletionItem, error) {
	result := make([]lsp.CompletionItem, 0, 200)
	parsedDoc, err := h.parsedDocuments.Get(h.uriToFilename(params.TextDocument.URI))
	if err == nil {
		di := DefinitionIndex{Line: int(params.Position.Line), Column: int(params.Position.Character)}
		for _, fn := range parsedDoc.Functions {
			if fn.BodyDefinition.InBBox(di) {
				for _, p := range fn.Parameters {
					ci, err := completionItemFromSymbol(p)
					if err != nil {
						continue
					}
					result = append(result, ci)
				}
				for _, p := range fn.LocalVariables {
					ci, err := completionItemFromSymbol(p)
					if err != nil {
						continue
					}
					result = append(result, ci)
				}
				break
			}
		}
	}
	h.parsedDocuments.WalkGlobalSymbols(func(s Symbol) error {
		ci, err := completionItemFromSymbol(s)
		if err != nil {
			return nil
		}
		result = append(result, ci)
		return nil
	}, SymbolAll)

	return result, nil
}

func (h *LspHandler) lookUpSymbol(documentURI string, position lsp.Position) (Symbol, error) {
	doc := h.bufferManager.GetBuffer(documentURI)
	if doc == "" {
		return nil, fmt.Errorf("document %q not found", documentURI)
	}
	identifier := doc.GetWordRangeAtPosition(position)

	p, err := h.parsedDocuments.Get(documentURI)
	if err == nil {
		di := DefinitionIndex{Line: int(position.Line), Column: int(position.Character)}
		for _, f := range p.Functions {
			if f.BodyDefinition.InBBox(di) {
				for _, param := range f.Parameters {
					if strings.EqualFold(param.Name(), identifier) {
						return param, nil
					}
				}
				for _, local := range f.LocalVariables {
					if strings.EqualFold(local.Name(), identifier) {
						return local, nil
					}
				}
			}
		}
	}

	symbol, found := h.parsedDocuments.LookupGlobalSymbol(strings.ToUpper(identifier), SymbolAll)

	if !found {
		return nil, fmt.Errorf("identifier %q not found", identifier)
	}

	return symbol, nil
}

func (h *LspHandler) handleSignatureInfo(ctx context.Context, params *lsp.TextDocumentPositionParams) (lsp.SignatureHelp, error) {
	doc := h.bufferManager.GetBuffer(h.uriToFilename(params.TextDocument.URI))
	methodCallLine := doc.GetMethodCall(params.Position)
	// The expected method call turned out to be a `func void something( ... )` -> a function definition
	if rxFunctionDef.MatchString(methodCallLine) {
		return lsp.SignatureHelp{}, nil
	}

	methodCallLine = rxStringValues.ReplaceAllLiteralString(methodCallLine, "")
	oldLen := -1
	for len(methodCallLine) != oldLen {
		oldLen = len(methodCallLine)
		methodCallLine = rxFuncCall.ReplaceAllLiteralString(methodCallLine, "")
	}

	// If for some reason the parenthesis of the methodcall went missing
	idxParen := strings.LastIndexByte(methodCallLine, '(')
	if idxParen < 0 {
		return lsp.SignatureHelp{}, fmt.Errorf("the parenthesis of the methodcall went missing")
	}

	word := ""
	for i := idxParen - 1; i > 0; i-- {
		if !isIdentifier(methodCallLine[i]) {
			start := i + 1
			if start+idxParen > len(methodCallLine) {
				return lsp.SignatureHelp{}, fmt.Errorf("idx out of bounds. Bad format :/")
			}
			word = methodCallLine[start : start+idxParen]
		}
	}
	if word == "" {
		word = methodCallLine[:idxParen]
	}
	word = strings.ToUpper(strings.TrimSpace(word))

	funcSymbol, found := h.parsedDocuments.LookupGlobalSymbol(word, SymbolFunction)
	if !found {
		return lsp.SignatureHelp{}, fmt.Errorf("no functino symbol found")
	}
	sigCtx := methodCallLine[idxParen+1:]
	fn := funcSymbol.(FunctionSymbol)

	var fnParams []lsp.ParameterInformation
	for _, p := range fn.Parameters {
		fnParams = append(fnParams, lsp.ParameterInformation{
			Label: p.String(),
		})
	}

	return lsp.SignatureHelp{
		Signatures: []lsp.SignatureInformation{
			{
				Documentation: &lsp.MarkupContent{
					Kind:  lsp.Markdown,
					Value: fn.Documentation(),
				},
				Label:      fn.String(),
				Parameters: fnParams,
			},
		},
		ActiveParameter: float64(strings.Count(sigCtx, ",")),
		ActiveSignature: 0,
	}, nil
}

func (h *LspHandler) handleGoToDefinition(ctx context.Context, params *lsp.TextDocumentPositionParams) (lsp.Location, error) {
	symbol, err := h.lookUpSymbol(h.uriToFilename(params.TextDocument.URI), params.Position)
	if err != nil {
		return lsp.Location{}, err
	}

	return lsp.Location{
		URI: uri.File(symbol.Source()),
		Range: lsp.Range{
			Start: lsp.Position{
				Character: float64(symbol.Definition().Start.Column),
				Line:      float64(symbol.Definition().Start.Line - 1),
			},
			End: lsp.Position{
				Character: float64(symbol.Definition().Start.Column + len(symbol.Name())),
				Line:      float64(symbol.Definition().Start.Line - 1),
			},
		}}, nil
}

// Deliver ...
func (h *LspHandler) Deliver(ctx context.Context, r *jsonrpc2.Request, delivered bool) bool {
	h.LogDebug("Requested '%s'\n", r.Method)
	if delivered {
		return false
	}

	// if r.Params != nil {
	// 	var paramsMap map[string]interface{}
	// 	json.Unmarshal(*r.Params, &paramsMap)
	// 	fmt.Fprintf(os.Stderr, "Params: %+v\n", paramsMap)
	// }
	switch r.Method {
	case lsp.MethodInitialize:
		if err := r.Reply(ctx, lsp.InitializeResult{
			Capabilities: lsp.ServerCapabilities{
				CompletionProvider: &lsp.CompletionOptions{
					TriggerCharacters: []string{"."},
				},
				DefinitionProvider: true,
				HoverProvider:      true,
				SignatureHelpProvider: &lsp.SignatureHelpOptions{
					TriggerCharacters: []string{"(", ","},
				},
				TextDocumentSync: lsp.TextDocumentSyncOptions{
					Change:    float64(lsp.Full),
					OpenClose: true,
					Save: &lsp.SaveOptions{
						IncludeText: true,
					},
				},
			},
		}, nil); err != nil {
			return false
		}
		h.initialized = true
		return true
	case lsp.MethodInitialized:
		go func() {
			exe, _ := os.Executable()
			resultsX, err := h.parsedDocuments.ParseSource(filepath.Join(filepath.Dir(exe), "DaedalusBuiltins", "builtins.src"))
			if err != nil {
				h.LogError("Error parsing %q: %v", filepath.Join(filepath.Dir(exe), "DaedalusBuiltins", "builtins.src"), err)
				return
			}

			for _, v := range []string{"Gothic.src", "Camera.src", "Menu.src", "Music.src", "ParticleFX.src", "SFX.src", "VisualFX.src"} {
				if _, err := os.Stat(v); err == nil {
					results, err := h.parsedDocuments.ParseSource(v)
					if err != nil {
						h.LogError("Error parsing %s: %v", v, err)
						return
					}
					resultsX = append(resultsX, results...)
				}
			}

			var diagnostics []lsp.Diagnostic
			tmpDiags := make(map[string][]lsp.Diagnostic)

			for _, p := range resultsX {
				if p.SyntaxErrors != nil && len(p.SyntaxErrors) > 0 {
					diagnostics = make([]lsp.Diagnostic, 0, len(p.SyntaxErrors))
					for _, se := range p.SyntaxErrors {
						diagnostics = append(diagnostics, se.Diagnostic())
					}
					tmpDiags[p.Source] = diagnostics
				}
			}
			h.initialDiagnostics = tmpDiags
		}()
		return true
	}

	// DEFAULT / OTHERWISE

	if !h.initialized {
		if !r.IsNotify() {
			r.Reply(ctx, nil, jsonrpc2.Errorf(jsonrpc2.ServerNotInitialized, "Not initialized yet"))
		}
		return false
	}

	// Recover if something bad happens in the handlers...
	defer func() {
		err := recover()
		if err != nil {
			h.LogWarn("Recovered from panic at %s: %v\n", r.Method, err)
		}
	}()
	if h.initialDiagnostics != nil && len(h.initialDiagnostics) > 0 {
		fmt.Fprintf(os.Stderr, "Publishing initial diagnostics (%d).\n", len(h.initialDiagnostics))
		for k, v := range h.initialDiagnostics {
			fmt.Fprintf(os.Stderr, "> %s\n", k)
			r.Conn().Notify(ctx, lsp.MethodTextDocumentPublishDiagnostics, lsp.PublishDiagnosticsParams{
				URI:         lsp.DocumentURI(uri.File(k)),
				Diagnostics: v,
			})
		}
		h.initialDiagnostics = map[string][]lsp.Diagnostic{}
	}
	switch r.Method {
	case lsp.MethodTextDocumentCompletion:
		var params lsp.CompletionParams
		json.Unmarshal(*r.Params, &params)
		items, err := h.handleTextDocumentCompletion(ctx, &params)
		h.replyEither(ctx, r, items, err)

	case lsp.MethodTextDocumentDefinition:
		var params lsp.TextDocumentPositionParams
		json.Unmarshal(*r.Params, &params)
		found, err := h.handleGoToDefinition(ctx, &params)
		if err != nil {
			h.replyEither(ctx, r, nil, nil)
		} else {
			h.replyEither(ctx, r, found, nil)
		}

	case lsp.MethodTextDocumentHover:
		var params lsp.TextDocumentPositionParams
		json.Unmarshal(*r.Params, &params)
		found, err := h.lookUpSymbol(h.uriToFilename(params.TextDocument.URI), params.Position)
		if err != nil {
			h.replyEither(ctx, r, nil, nil)
		} else {
			h.LogDebug("Found Symbol for Hover: %s\n", found.String())
			h.replyEither(ctx, r, lsp.Hover{
				Range: lsp.Range{
					Start: params.Position,
					End:   params.Position,
				},
				Contents: lsp.MarkupContent{
					Kind:  lsp.Markdown,
					Value: strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(found.Documentation(), "\r", ""), "\n", "  \n") + "\n```daedalus\n" + found.String() + "\n```"),
				},
			}, nil)
		}

	case lsp.MethodTextDocumentSignatureHelp:
		var params lsp.TextDocumentPositionParams
		json.Unmarshal(*r.Params, &params)
		result, err := h.handleSignatureInfo(ctx, &params)
		if err == nil {
			r.Reply(ctx, result, nil)
		} else {
			r.Reply(ctx, nil, nil)
		}
	default:
		return h.baseLspHandler.Deliver(ctx, r, delivered)
	}
	return true
}
