package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MCP-сервер (JSON-RPC 2.0 через stdio): дає агентові інструменти для роботи з
// віддаленим екраном. Координати в усіх інструментах — у пікселях зменшеного
// знімка, який бачить модель; сервер сам перераховує їх у справжні.

var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

type rpcRequest struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	input       bool           // змінює щось на віддаленому ПК (недоступно в режимі перегляду)
}

type mcpServer struct {
	vnc      *vncClient
	viewOnly bool
	mu       sync.Mutex // один виклик інструмента за раз
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, desc string) map[string]any {
	return map[string]any{"type": typ, "description": desc}
}

var after = map[string]any{"type": "boolean",
	"description": "Return a fresh screenshot after the action (default true). Set false when chaining several actions."}

func (m *mcpServer) tools() []tool {
	xy := func(extra map[string]any) map[string]any {
		p := map[string]any{
			"x": prop("integer", "X in screenshot pixels"),
			"y": prop("integer", "Y in screenshot pixels"),
		}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	all := []tool{
		{Name: "screenshot", Title: "Take a screenshot",
			Description: "Capture the remote Windows screen. Returns an image scaled to fit the model; every coordinate in the other tools refers to this image's pixels (origin top-left). Call this first, and again whenever you need to see the current state.",
			InputSchema: obj(map[string]any{}),
			Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "zoom", Title: "Zoom into a region",
			Description: "Return a region of the screen at full resolution, to read small text or inspect details. The region is given in screenshot pixels. Coordinates for other tools still use the full screenshot, not the zoomed image.",
			InputSchema: obj(map[string]any{
				"x0": prop("integer", "Left edge, screenshot pixels"), "y0": prop("integer", "Top edge, screenshot pixels"),
				"x1": prop("integer", "Right edge, screenshot pixels"), "y1": prop("integer", "Bottom edge, screenshot pixels"),
			}, "x0", "y0", "x1", "y1"),
			Annotations: map[string]any{"readOnlyHint": true}},
		{Name: "click", Title: "Click the mouse", input: true,
			Description: "Move the mouse to a point and click. Use count 2 for a double click, 3 for a triple click.",
			InputSchema: obj(xy(map[string]any{
				"button":     map[string]any{"type": "string", "enum": []string{"left", "right", "middle"}, "description": "Mouse button (default left)"},
				"count":      map[string]any{"type": "integer", "minimum": 1, "maximum": 3, "description": "Number of clicks (default 1)"},
				"modifiers":  prop("string", "Keys held during the click, e.g. \"ctrl\" or \"ctrl+shift\""),
				"screenshot": after,
			}), "x", "y")},
		{Name: "move_mouse", Title: "Move the mouse", input: true,
			Description: "Move the mouse pointer without clicking, e.g. to hover over an element.",
			InputSchema: obj(xy(map[string]any{"screenshot": after}), "x", "y")},
		{Name: "drag", Title: "Drag with the mouse", input: true,
			Description: "Press the left button at one point, move to another and release.",
			InputSchema: obj(map[string]any{
				"from_x": prop("integer", "Start X, screenshot pixels"), "from_y": prop("integer", "Start Y, screenshot pixels"),
				"to_x": prop("integer", "End X, screenshot pixels"), "to_y": prop("integer", "End Y, screenshot pixels"),
				"screenshot": after,
			}, "from_x", "from_y", "to_x", "to_y")},
		{Name: "scroll", Title: "Scroll the mouse wheel", input: true,
			Description: "Scroll at a point. Amount is the number of wheel notches (default 3).",
			InputSchema: obj(xy(map[string]any{
				"direction":  map[string]any{"type": "string", "enum": []string{"up", "down", "left", "right"}, "description": "Scroll direction"},
				"amount":     map[string]any{"type": "integer", "minimum": 1, "maximum": 30, "description": "Wheel notches (default 3)"},
				"screenshot": after,
			}), "x", "y", "direction")},
		{Name: "type_text", Title: "Type text", input: true,
			Description: "Type text at the current keyboard focus, exactly as given, in any language. Newlines press Enter. Click the target field first if it is not focused.",
			InputSchema: obj(map[string]any{
				"text":       prop("string", "Text to type"),
				"screenshot": after,
			}, "text")},
		{Name: "press_keys", Title: "Press a key or shortcut", input: true,
			Description: "Press a key or key combination, e.g. \"Enter\", \"ctrl+c\", \"alt+F4\", \"win+r\", \"shift+Tab\". Modifiers: ctrl, alt, shift, win. Keys: letters, digits, Enter, Tab, Esc, Backspace, Delete, Insert, Home, End, PageUp, PageDown, arrows (Left/Right/Up/Down), Space, F1-F24.",
			InputSchema: obj(map[string]any{
				"keys":       prop("string", "Key or combination joined with +"),
				"repeat":     map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "How many times to press (default 1)"},
				"screenshot": after,
			}, "keys")},
	}
	if !m.viewOnly {
		return all
	}
	var ro []tool
	for _, t := range all {
		if !t.input {
			ro = append(ro, t)
		}
	}
	return ro
}

// serve читає повідомлення MCP з in (по одному в рядку) і пише відповіді в out.
func (m *mcpServer) serve(in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if resp := m.handle(bytes.TrimSpace(line)); resp != nil {
				b, _ := json.Marshal(resp) // json.Marshal не лишає в рядку переводів рядка
				if _, werr := out.Write(append(b, '\n')); werr != nil {
					return werr
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil // клієнт закрив stdin — завершуємось
			}
			return err
		}
	}
}

func (m *mcpServer) handle(line []byte) *rpcResponse {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return &rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}}
	}
	if len(req.ID) == 0 {
		return nil // сповіщення (наприклад notifications/initialized) відповіді не потребує
	}
	resp := &rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		version := protocolVersions[0]
		for _, v := range protocolVersions {
			if v == p.ProtocolVersion {
				version = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "simplevnc", "title": "SimpleVNC remote desktop", "version": "1.0.0"},
			"instructions": "Controls a remote Windows desktop over VNC. Take a screenshot first; all coordinates are in screenshot pixels. " +
				"Use zoom to read small text. Action tools return a fresh screenshot unless screenshot=false. " +
				"Windows permission prompts (UAC) and the lock screen must be answered by a person, not by you.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": m.tools()}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			resp.Error = &rpcError{-32602, "invalid params"}
			break
		}
		known := false
		for _, t := range m.tools() {
			known = known || t.Name == p.Name
		}
		if !known {
			resp.Error = &rpcError{-32602, "Unknown tool: " + p.Name}
			break
		}
		m.mu.Lock()
		res, err := m.call(p.Name, p.Arguments)
		m.mu.Unlock()
		if err != nil {
			res = &toolResult{Content: []content{{Type: "text", Text: err.Error()}}, IsError: true}
		}
		resp.Result = res
	default:
		resp.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	return resp
}

// arg дістає ціле число з аргументів (JSON-числа приходять як float64).
func arg(a map[string]any, key string, def int) (int, error) {
	v, ok := a[key]
	if !ok || v == nil {
		return def, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("argument %q must be a number", key)
	}
	return int(math.Round(f)), nil
}

func need(a map[string]any, keys ...string) ([]int, error) {
	out := make([]int, len(keys))
	for i, k := range keys {
		if _, ok := a[k]; !ok {
			return nil, fmt.Errorf("missing argument %q", k)
		}
		v, err := arg(a, k, 0)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// fit — розмір знімка для моделі: довша сторона до 1568 пікселів і до ~1,15 Мп.
// Більше зображення модель зменшила б сама, і координати перестали б збігатися.
func fit(w, h int) (int, int) {
	const maxEdge, maxPixels = 1568.0, 1_150_000.0
	s := 1.0
	if m := float64(max(w, h)); m > maxEdge {
		s = maxEdge / m
	}
	if float64(w*h)*s*s > maxPixels {
		s = math.Sqrt(maxPixels / float64(w*h))
	}
	return max(int(float64(w)*s+0.5), 1), max(int(float64(h)*s+0.5), 1)
}

// shrink зменшує RGBA-зображення усередненням блоків пікселів.
func shrink(src []byte, sw, sh, dw, dh int) []byte {
	if dw == sw && dh == sh {
		return src
	}
	dst := make([]byte, dw*dh*4)
	for y := 0; y < dh; y++ {
		y0, y1 := y*sh/dh, max((y+1)*sh/dh, y*sh/dh+1)
		for x := 0; x < dw; x++ {
			x0, x1 := x*sw/dw, max((x+1)*sw/dw, x*sw/dw+1)
			var r, g, b, n int
			for yy := y0; yy < y1; yy++ {
				row := src[(yy*sw+x0)*4 : (yy*sw+x1)*4]
				for i := 0; i < len(row); i += 4 {
					r, g, b, n = r+int(row[i]), g+int(row[i+1]), b+int(row[i+2]), n+1
				}
			}
			o := (y*dw + x) * 4
			dst[o], dst[o+1], dst[o+2], dst[o+3] = byte(r/n), byte(g/n), byte(b/n), 255
		}
	}
	return dst
}

func pngImage(rgba []byte, w, h int) content {
	for i := 3; i < len(rgba); i += 4 {
		rgba[i] = 255 // четвертий байт від сервера — не прозорість
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	enc.Encode(&buf, &image.RGBA{Pix: rgba, Stride: w * 4, Rect: image.Rect(0, 0, w, h)})
	return content{Type: "image", Data: base64.StdEncoding.EncodeToString(buf.Bytes()), MimeType: "image/png"}
}

// shot — знімок екрана як вміст відповіді інструмента.
func (m *mcpServer) shot() ([]content, error) {
	fb, w, h, err := m.vnc.snapshot(15 * time.Second)
	if err != nil {
		return nil, err
	}
	sw, sh := fit(w, h)
	return []content{
		pngImage(shrink(fb, w, h, sw, sh), sw, sh),
		{Type: "text", Text: fmt.Sprintf("Screenshot %dx%d of a %dx%d screen. Coordinates for all tools are in this %dx%d image, origin top-left.", sw, sh, w, h, sw, sh)},
	}, nil
}

// real переводить точку зі знімка у справжні пікселі екрана.
func (m *mcpServer) real(x, y int) (int, int, error) {
	if _, _, _, err := m.vnc.snapshot(15 * time.Second); err != nil {
		return 0, 0, err
	}
	w, h := m.vnc.size()
	sw, sh := fit(w, h)
	if x < 0 || y < 0 || x >= sw || y >= sh {
		return 0, 0, fmt.Errorf("point (%d,%d) is outside the %dx%d screenshot", x, y, sw, sh)
	}
	return min((x*w+w/2)/sw, w-1), min((y*h+h/2)/sh, h-1), nil
}

func (m *mcpServer) call(name string, a map[string]any) (*toolResult, error) {
	if a == nil {
		a = map[string]any{}
	}
	v := m.vnc
	pause := func(ms int) { time.Sleep(time.Duration(ms) * time.Millisecond) }
	var done string

	switch name {
	case "screenshot":
		v.settle(0, 120*time.Millisecond, 400*time.Millisecond)
		c, err := m.shot()
		if err != nil {
			return nil, err
		}
		return &toolResult{Content: c}, nil

	case "zoom":
		p, err := need(a, "x0", "y0", "x1", "y1")
		if err != nil {
			return nil, err
		}
		fb, w, h, err := v.snapshot(15 * time.Second)
		if err != nil {
			return nil, err
		}
		sw, sh := fit(w, h)
		x0, y0 := max(min(p[0], p[2]), 0)*w/sw, max(min(p[1], p[3]), 0)*h/sh
		x1, y1 := min(max(p[0], p[2]), sw)*w/sw, min(max(p[1], p[3]), sh)*h/sh
		if x1-x0 < 2 || y1-y0 < 2 {
			return nil, fmt.Errorf("the region is empty")
		}
		cw, ch := x1-x0, y1-y0
		crop := make([]byte, cw*ch*4)
		for row := 0; row < ch; row++ {
			copy(crop[row*cw*4:], fb[((y0+row)*w+x0)*4:((y0+row)*w+x1)*4])
		}
		for i := 3; i < len(crop); i += 4 {
			crop[i] = 255
		}
		zw, zh := fit(cw, ch)
		return &toolResult{Content: []content{
			pngImage(shrink(crop, cw, ch, zw, zh), zw, zh),
			{Type: "text", Text: fmt.Sprintf("Region (%d,%d)-(%d,%d) of the screenshot, shown at %dx%d (%.1fx magnification). Other tools still use screenshot coordinates.",
				p[0], p[1], p[2], p[3], zw, zh, float64(zw)/float64(max(abs(p[2]-p[0]), 1)))},
		}}, nil

	case "click":
		p, err := need(a, "x", "y")
		if err != nil {
			return nil, err
		}
		x, y, err := m.real(p[0], p[1])
		if err != nil {
			return nil, err
		}
		mask := byte(1)
		button, _ := a["button"].(string)
		switch button {
		case "", "left":
			button = "left"
		case "middle":
			mask = 2
		case "right":
			mask = 4
		default:
			return nil, fmt.Errorf("unknown button %q", button)
		}
		count, err := arg(a, "count", 1)
		if err != nil || count < 1 || count > 3 {
			return nil, fmt.Errorf("count must be 1, 2 or 3")
		}
		var mods []uint32
		if s, _ := a["modifiers"].(string); s != "" {
			var key uint32
			if mods, key, err = parseCombo(s); err != nil || key != 0 {
				return nil, fmt.Errorf("modifiers must be ctrl, alt, shift or win joined with +")
			}
		}
		for _, k := range mods {
			v.key(k, true)
		}
		if err := v.pointer(x, y, 0); err != nil {
			return nil, err
		}
		pause(40)
		for i := 0; i < count; i++ {
			v.pointer(x, y, mask)
			pause(40)
			v.pointer(x, y, 0)
			pause(70)
		}
		for i := len(mods) - 1; i >= 0; i-- {
			v.key(mods[i], false)
		}
		done = fmt.Sprintf("Clicked %s button %d time(s) at (%d,%d).", button, count, p[0], p[1])

	case "move_mouse":
		p, err := need(a, "x", "y")
		if err != nil {
			return nil, err
		}
		x, y, err := m.real(p[0], p[1])
		if err != nil {
			return nil, err
		}
		if err := v.pointer(x, y, 0); err != nil {
			return nil, err
		}
		done = fmt.Sprintf("Moved the mouse to (%d,%d).", p[0], p[1])

	case "drag":
		p, err := need(a, "from_x", "from_y", "to_x", "to_y")
		if err != nil {
			return nil, err
		}
		x0, y0, err := m.real(p[0], p[1])
		if err != nil {
			return nil, err
		}
		x1, y1, err := m.real(p[2], p[3])
		if err != nil {
			return nil, err
		}
		if err := v.pointer(x0, y0, 0); err != nil {
			return nil, err
		}
		pause(60)
		v.pointer(x0, y0, 1)
		pause(120)
		for i := 1; i <= 8; i++ { // поступовий рух: багато програм не помічають «стрибка»
			v.pointer(x0+(x1-x0)*i/8, y0+(y1-y0)*i/8, 1)
			pause(25)
		}
		pause(80)
		v.pointer(x1, y1, 0)
		done = fmt.Sprintf("Dragged from (%d,%d) to (%d,%d).", p[0], p[1], p[2], p[3])

	case "scroll":
		p, err := need(a, "x", "y")
		if err != nil {
			return nil, err
		}
		x, y, err := m.real(p[0], p[1])
		if err != nil {
			return nil, err
		}
		dir, _ := a["direction"].(string)
		bit, ok := map[string]byte{"up": 8, "down": 16, "left": 32, "right": 64}[dir]
		if !ok {
			return nil, fmt.Errorf("direction must be up, down, left or right")
		}
		amount, err := arg(a, "amount", 3)
		if err != nil || amount < 1 || amount > 30 {
			return nil, fmt.Errorf("amount must be between 1 and 30")
		}
		if err := v.pointer(x, y, 0); err != nil {
			return nil, err
		}
		for i := 0; i < amount; i++ {
			v.pointer(x, y, bit)
			v.pointer(x, y, 0)
			pause(15)
		}
		done = fmt.Sprintf("Scrolled %s by %d at (%d,%d).", dir, amount, p[0], p[1])

	case "type_text":
		text, _ := a["text"].(string)
		if text == "" {
			return nil, fmt.Errorf("text is empty")
		}
		if utf8.RuneCountInString(text) > 5000 {
			return nil, fmt.Errorf("text is longer than 5000 characters; split it into several calls")
		}
		for _, r := range strings.ReplaceAll(text, "\r\n", "\n") {
			sym := runeKeysym(r)
			if err := v.key(sym, true); err != nil {
				return nil, err
			}
			v.key(sym, false)
			pause(4)
		}
		done = fmt.Sprintf("Typed %d characters.", utf8.RuneCountInString(text))

	case "press_keys":
		keys, _ := a["keys"].(string)
		mods, key, err := parseCombo(keys)
		if err != nil {
			return nil, err
		}
		repeat, err := arg(a, "repeat", 1)
		if err != nil || repeat < 1 || repeat > 50 {
			return nil, fmt.Errorf("repeat must be between 1 and 50")
		}
		for i := 0; i < repeat; i++ {
			for _, k := range mods {
				if err := v.key(k, true); err != nil {
					return nil, err
				}
			}
			if key != 0 {
				v.key(key, true)
				pause(20)
				v.key(key, false)
			}
			for j := len(mods) - 1; j >= 0; j-- {
				v.key(mods[j], false)
			}
			pause(30)
		}
		done = fmt.Sprintf("Pressed %s %d time(s).", keys, repeat)
	}

	logf("дія агента: %s", strings.SplitN(done, " at ", 2)[0])
	res := &toolResult{Content: []content{{Type: "text", Text: done}}}
	if want, ok := a["screenshot"].(bool); !ok || want {
		v.settle(250*time.Millisecond, 150*time.Millisecond, 1500*time.Millisecond)
		if c, err := m.shot(); err == nil {
			res.Content = append(res.Content, c...)
		}
	}
	return res, nil
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
