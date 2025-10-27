package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)


type LocaleCode string


const (
	LocaleEN LocaleCode = "en"
	LocaleES LocaleCode = "es"
	LocaleFR LocaleCode = "fr"
	LocaleDE LocaleCode = "de"
	LocaleIT LocaleCode = "it"
	LocalePT LocaleCode = "pt"
	LocaleRU LocaleCode = "ru"
	LocaleJA LocaleCode = "ja"
	LocaleKO LocaleCode = "ko"
	LocaleZH LocaleCode = "zh"
)


type EngineConfig struct {
	APIKey            string
	APIURL            string
	BatchSize         int
	IdealBatchItemSize int
}


func DefaultEngineConfig() *EngineConfig {
	return &EngineConfig{
		APIURL:            "https://engine.lingo.dev",
		BatchSize:         25,
		IdealBatchItemSize: 250,
	}
}


type LocalizationParams struct {
	SourceLocale *LocaleCode           `json:"sourceLocale,omitempty"`
	TargetLocale LocaleCode            `json:"targetLocale"`
	Fast         *bool                 `json:"fast,omitempty"`
	Reference    map[LocaleCode]map[string]interface{} `json:"reference,omitempty"`
	Hints        map[string][]string   `json:"hints,omitempty"`
}


type BatchLocalizeTextParams struct {
	SourceLocale  LocaleCode   `json:"sourceLocale"`
	TargetLocales []LocaleCode `json:"targetLocales"`
	Fast          *bool        `json:"fast,omitempty"`
}


type ChatMessage struct {
	Name string `json:"name"`
	Text string `json:"text"`
}


type ProgressCallback func(progress int, sourceChunk, processedChunk map[string]string)


type SimpleProgressCallback func(progress int)


type WhoAmIResponse struct {
	Email string `json:"email"`
	ID    string `json:"id"`
}


type LingoDotDevEngine struct {
	config     *EngineConfig
	httpClient *http.Client
}


func NewEngine(config *EngineConfig) *LingoDotDevEngine {
	if config.APIURL == "" {
		config.APIURL = "https://engine.lingo.dev"
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 25
	}
	if config.IdealBatchItemSize <= 0 {
		config.IdealBatchItemSize = 250
	}

	return &LingoDotDevEngine{
		config: config,
		httpClient: &http.Client{
			Timeout: 300 * time.Second, 
		},
	}
}


func generateWorkflowID() string {
	return fmt.Sprintf("workflow_%d", time.Now().UnixNano())
}


func (e *LingoDotDevEngine) countWordsInPayload(payload interface{}) int {
	switch v := payload.(type) {
	case map[string]interface{}:
		count := 0
		for _, value := range v {
			count += e.countWordsInPayload(value)
		}
		return count
	case []interface{}:
		count := 0
		for _, item := range v {
			count += e.countWordsInPayload(item)
		}
		return count
	case string:
		if strings.TrimSpace(v) == "" {
			return 0
		}
		words := strings.Fields(strings.TrimSpace(v))
		return len(words)
	default:
		return 0
	}
}


func (e *LingoDotDevEngine) extractPayloadChunks(payload map[string]string) []map[string]string {
	var result []map[string]string
	currentChunk := make(map[string]string)
	currentChunkItemCount := 0

	for key, value := range payload {
		currentChunk[key] = value
		currentChunkItemCount++

		currentChunkSize := e.countWordsInPayload(currentChunk)
		if currentChunkSize > e.config.IdealBatchItemSize ||
			currentChunkItemCount >= e.config.BatchSize {
			result = append(result, currentChunk)
			currentChunk = make(map[string]string)
			currentChunkItemCount = 0
		}
	}

	if len(currentChunk) > 0 {
		result = append(result, currentChunk)
	}

	return result
}


func (e *LingoDotDevEngine) localizeChunk(ctx context.Context, sourceLocale *LocaleCode, targetLocale LocaleCode, payload map[string]interface{}, workflowID string, fast bool) (map[string]string, error) {
	requestBody := map[string]interface{}{
		"params": map[string]interface{}{
			"workflowId": workflowID,
			"fast":       fast,
		},
		"locale": map[string]interface{}{
			"source": sourceLocale,
			"target": targetLocale,
		},
		"data": payload["data"],
	}

	if ref, ok := payload["reference"]; ok {
		requestBody["reference"] = ref
	}
	if hints, ok := payload["hints"]; ok {
		requestBody["hints"] = hints
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.config.APIURL+"/i18n", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+e.config.APIKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	if !isSuccessStatusCode(resp.StatusCode) {
		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			return nil, fmt.Errorf("server error (%d): %s. %s. This may be due to temporary service issues", resp.StatusCode, resp.Status, string(body))
		} else if resp.StatusCode == 400 {
			return nil, fmt.Errorf("invalid request: %s", resp.Status)
		} else {
			return nil, fmt.Errorf("request failed: %s", string(body))
		}
	}

	var jsonResponse struct {
		Data  map[string]string `json:"data"`
		Error string            `json:"error"`
	}

	if err := json.Unmarshal(body, &jsonResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if jsonResponse.Error != "" {
		return nil, fmt.Errorf("API error: %s", jsonResponse.Error)
	}

	if jsonResponse.Data == nil {
		return make(map[string]string), nil
	}

	return jsonResponse.Data, nil
}


func (e *LingoDotDevEngine) localizeRaw(ctx context.Context, payload map[string]string, params *LocalizationParams, progressCallback ProgressCallback) (map[string]string, error) {
	chunkedPayload := e.extractPayloadChunks(payload)
	var processedPayloadChunks []map[string]string

	workflowID := generateWorkflowID()
	fast := false
	if params.Fast != nil {
		fast = *params.Fast
	}

	for i, chunk := range chunkedPayload {
		percentageCompleted := int(float64(i+1) / float64(len(chunkedPayload)) * 100)

		chunkPayload := map[string]interface{}{
			"data": chunk,
		}
		if params.Reference != nil {
			chunkPayload["reference"] = params.Reference
		}
		if params.Hints != nil {
			chunkPayload["hints"] = params.Hints
		}

		processedPayloadChunk, err := e.localizeChunk(ctx, params.SourceLocale, params.TargetLocale, chunkPayload, workflowID, fast)
		if err != nil {
			return nil, err
		}

		if progressCallback != nil {
			progressCallback(percentageCompleted, chunk, processedPayloadChunk)
		}

		processedPayloadChunks = append(processedPayloadChunks, processedPayloadChunk)
	}

	
	result := make(map[string]string)
	for _, chunk := range processedPayloadChunks {
		for k, v := range chunk {
			result[k] = v
		}
	}

	return result, nil
}


func (e *LingoDotDevEngine) LocalizeText(ctx context.Context, text string, params *LocalizationParams, progressCallback SimpleProgressCallback) (string, error) {
	var wrappedCallback ProgressCallback
	if progressCallback != nil {
		wrappedCallback = func(progress int, sourceChunk, processedChunk map[string]string) {
			progressCallback(progress)
		}
	}

	response, err := e.localizeRaw(ctx, map[string]string{"text": text}, params, wrappedCallback)
	if err != nil {
		return "", err
	}

	if result, ok := response["text"]; ok {
		return result, nil
	}
	return "", nil
}


func (e *LingoDotDevEngine) BatchLocalizeText(ctx context.Context, text string, params *BatchLocalizeTextParams) ([]string, error) {
	responses := make([]string, len(params.TargetLocales))
	
	for i, targetLocale := range params.TargetLocales {
		locParams := &LocalizationParams{
			SourceLocale: &params.SourceLocale,
			TargetLocale: targetLocale,
			Fast:         params.Fast,
		}
		
		result, err := e.LocalizeText(ctx, text, locParams, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to localize to %s: %w", targetLocale, err)
		}
		responses[i] = result
	}

	return responses, nil
}


func (e *LingoDotDevEngine) LocalizeObject(ctx context.Context, obj map[string]interface{}, params *LocalizationParams, progressCallback ProgressCallback) (map[string]interface{}, error) {
	
	stringMap := make(map[string]string)
	flattenObject(obj, "", stringMap)

	localizedStringMap, err := e.localizeRaw(ctx, stringMap, params, progressCallback)
	if err != nil {
		return nil, err
	}

	
	result := make(map[string]interface{})
	unflattenObject(localizedStringMap, result)

	return result, nil
}


func (e *LingoDotDevEngine) LocalizeStringArray(ctx context.Context, strings []string, params *LocalizationParams) ([]string, error) {
	mapped := make(map[string]string)
	for i, str := range strings {
		mapped[fmt.Sprintf("item_%d", i)] = str
	}

	obj := make(map[string]interface{})
	for k, v := range mapped {
		obj[k] = v
	}

	result, err := e.LocalizeObject(ctx, obj, params, nil)
	if err != nil {
		return nil, err
	}

	localizedStrings := make([]string, len(strings))
	for i := range strings {
		key := fmt.Sprintf("item_%d", i)
		if val, ok := result[key].(string); ok {
			localizedStrings[i] = val
		}
	}

	return localizedStrings, nil
}


func (e *LingoDotDevEngine) LocalizeChat(ctx context.Context, chat []ChatMessage, params *LocalizationParams, progressCallback SimpleProgressCallback) ([]ChatMessage, error) {
	chatMap := make(map[string]string)
	for i, msg := range chat {
		chatMap[fmt.Sprintf("chat_%d", i)] = msg.Text
	}

	var wrappedCallback ProgressCallback
	if progressCallback != nil {
		wrappedCallback = func(progress int, sourceChunk, processedChunk map[string]string) {
			progressCallback(progress)
		}
	}

	localized, err := e.localizeRaw(ctx, chatMap, params, wrappedCallback)
	if err != nil {
		return nil, err
	}

	result := make([]ChatMessage, len(chat))
	for i, msg := range chat {
		key := fmt.Sprintf("chat_%d", i)
		if localizedText, ok := localized[key]; ok {
			result[i] = ChatMessage{
				Name: msg.Name,
				Text: localizedText,
			}
		} else {
			result[i] = msg 
		}
	}

	return result, nil
}


func (e *LingoDotDevEngine) LocalizeHTML(ctx context.Context, htmlContent string, params *LocalizationParams, progressCallback SimpleProgressCallback) (string, error) {
	doc, err := html.Parse(strings.NewReader(htmlContent))
	if err != nil {
		return "", fmt.Errorf("failed to parse HTML: %w", err)
	}

	extractedContent := make(map[string]string)
	extractHTMLContent(doc, extractedContent, "")

	var wrappedCallback ProgressCallback
	if progressCallback != nil {
		wrappedCallback = func(progress int, sourceChunk, processedChunk map[string]string) {
			progressCallback(progress)
		}
	}

	localizedContent, err := e.localizeRaw(ctx, extractedContent, params, wrappedCallback)
	if err != nil {
		return "", err
	}

	updateHTMLContent(doc, localizedContent)

	
	updateLangAttribute(doc, string(params.TargetLocale))

	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return "", fmt.Errorf("failed to render HTML: %w", err)
	}

	return buf.String(), nil
}


func (e *LingoDotDevEngine) RecognizeLocale(ctx context.Context, text string) (LocaleCode, error) {
	requestBody := map[string]string{
		"text": text,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.config.APIURL+"/recognize", bytes.NewBuffer(jsonData))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+e.config.APIKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if !isSuccessStatusCode(resp.StatusCode) {
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			return "", fmt.Errorf("server error (%d): %s. This may be due to temporary service issues", resp.StatusCode, resp.Status)
		}
		return "", fmt.Errorf("error recognizing locale: %s", string(body))
	}

	var jsonResponse struct {
		Locale LocaleCode `json:"locale"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&jsonResponse); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	return jsonResponse.Locale, nil
}


func (e *LingoDotDevEngine) WhoAmI(ctx context.Context) (*WhoAmIResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", e.config.APIURL+"/whoami", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+e.config.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if !isSuccessStatusCode(resp.StatusCode) {
		if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("server error (%d): %s. This may be due to temporary service issues", resp.StatusCode, string(body))
		}
		return nil, nil
	}

	var response WhoAmIResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if response.Email == "" {
		return nil, nil
	}

	return &response, nil
}



func isSuccessStatusCode(statusCode int) bool {
	return statusCode >= 200 && statusCode < 300
}

func flattenObject(obj interface{}, prefix string, result map[string]string) {
	switch v := obj.(type) {
	case map[string]interface{}:
		for key, value := range v {
			newKey := key
			if prefix != "" {
				newKey = prefix + "." + key
			}
			flattenObject(value, newKey, result)
		}
	case []interface{}:
		for i, value := range v {
			newKey := strconv.Itoa(i)
			if prefix != "" {
				newKey = prefix + "." + newKey
			}
			flattenObject(value, newKey, result)
		}
	case string:
		if strings.TrimSpace(v) != "" {
			result[prefix] = v
		}
	}
}

func unflattenObject(flat map[string]string, result map[string]interface{}) {
	for key, value := range flat {
		keys := strings.Split(key, ".")
		current := result

		for i, k := range keys[:len(keys)-1] {
			if _, exists := current[k]; !exists {
				
				if i+1 < len(keys) {
					if _, err := strconv.Atoi(keys[i+1]); err == nil {
						current[k] = make([]interface{}, 0)
					} else {
						current[k] = make(map[string]interface{})
					}
				} else {
					current[k] = make(map[string]interface{})
				}
			}

			switch next := current[k].(type) {
			case map[string]interface{}:
				current = next
			case []interface{}:
				
				current[k] = make(map[string]interface{})
				current = current[k].(map[string]interface{})
			}
		}

		finalKey := keys[len(keys)-1]
		current[finalKey] = value
	}
}

var localizableAttributes = map[string][]string{
	"meta":  {"content"},
	"img":   {"alt"},
	"input": {"placeholder"},
	"a":     {"title"},
}

var unlocalizableTags = map[string]bool{
	"script": true,
	"style":  true,
}

func extractHTMLContent(n *html.Node, extracted map[string]string, path string) {
	if n.Type == html.TextNode {
		text := strings.TrimSpace(n.Data)
		if text != "" && !isInUnlocalizableTag(n) {
			extracted[path] = text
		}
	} else if n.Type == html.ElementNode {
		
		if attrs, ok := localizableAttributes[n.Data]; ok {
			for _, attr := range attrs {
				for _, a := range n.Attr {
					if a.Key == attr && strings.TrimSpace(a.Val) != "" {
						extracted[path+"#"+attr] = a.Val
					}
				}
			}
		}

		childIndex := 0
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode || (c.Type == html.TextNode && strings.TrimSpace(c.Data) != "") {
				childPath := fmt.Sprintf("%s/%s[%d]", path, c.Data, childIndex)
				if c.Type == html.TextNode {
					childPath = fmt.Sprintf("%s/text[%d]", path, childIndex)
				}
				extractHTMLContent(c, extracted, childPath)
				childIndex++
			}
		}
	}

	if path == "" { 
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				extractHTMLContent(c, extracted, c.Data)
			}
		}
	}
}

func updateHTMLContent(n *html.Node, localized map[string]string) {
	
	
	if n.Type == html.TextNode {
		for path, localizedText := range localized {
			if !strings.Contains(path, "#") {
				n.Data = localizedText
				break
			}
		}
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		updateHTMLContent(c, localized)
	}
}

func updateLangAttribute(n *html.Node, lang string) {
	if n.Type == html.ElementNode && n.Data == "html" {
		
		found := false
		for i, attr := range n.Attr {
			if attr.Key == "lang" {
				n.Attr[i].Val = lang
				found = true
				break
			}
		}
		if !found {
			n.Attr = append(n.Attr, html.Attribute{Key: "lang", Val: lang})
		}
		return
	}

	for c := n.FirstChild; c != nil; c = c.NextSibling {
		updateLangAttribute(c, lang)
	}
}

func isInUnlocalizableTag(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && unlocalizableTags[p.Data] {
			return true
		}
	}
	return false
}