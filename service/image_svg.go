package service

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const svgNamespace = "http://www.w3.org/2000/svg"

// SVG outputs are passive documents. This intentionally excludes scripting,
// animation, embedded documents/images, network resources and arbitrary CSS.
var svgOutputElements = strings.Fields(`svg g defs symbol use title desc path rect circle ellipse line polyline polygon
	text tspan textPath linearGradient radialGradient stop pattern clipPath mask marker
	filter feBlend feColorMatrix feComponentTransfer feComposite feConvolveMatrix feDiffuseLighting
	feDisplacementMap feDistantLight feDropShadow feFlood feFuncA feFuncB feFuncG feFuncR feGaussianBlur
	feMerge feMergeNode feMorphology feOffset fePointLight feSpecularLighting feSpotLight feTile feTurbulence style`)

var svgOutputAttributes = strings.Fields(`id class version width height viewBox preserveAspectRatio
	x y x1 y1 x2 y2 dx dy cx cy r rx ry d points pathLength transform opacity
	fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap stroke-linejoin stroke-miterlimit
	stroke-dasharray stroke-dashoffset clip-rule clip-path mask filter color color-interpolation color-interpolation-filters
	display visibility overflow vector-effect paint-order shape-rendering text-rendering image-rendering
	font-family font-size font-weight font-style font-variant font-stretch font-kerning
	text-anchor dominant-baseline alignment-baseline baseline-shift letter-spacing word-spacing text-decoration
	textLength lengthAdjust rotate startOffset method spacing
	gradientUnits gradientTransform spreadMethod offset stop-color stop-opacity fx fy fr
	patternUnits patternContentUnits patternTransform clipPathUnits maskUnits maskContentUnits
	marker-start marker-mid marker-end markerWidth markerHeight markerUnits orient refX refY
	filterUnits primitiveUnits in in2 result mode type values tableValues slope intercept amplitude exponent
	k1 k2 k3 k4 operator order kernelMatrix divisor bias targetX targetY edgeMode kernelUnitLength preserveAlpha
	surfaceScale diffuseConstant specularConstant specularExponent limitingConeAngle azimuth elevation
	z pointsAtX pointsAtY pointsAtZ scale xChannelSelector yChannelSelector stdDeviation radius
	baseFrequency numOctaves seed stitchTiles flood-color flood-opacity lighting-color
	role aria-label aria-labelledby aria-describedby focusable tabindex`)

var svgOutputCSSProperties = strings.Fields(`fill fill-opacity fill-rule stroke stroke-width stroke-opacity stroke-linecap
	stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity color clip-rule clip-path mask filter
	color-interpolation color-interpolation-filters display visibility overflow vector-effect paint-order shape-rendering
	text-rendering image-rendering font-family font-size font-weight font-style font-variant font-stretch font-kerning
	text-anchor dominant-baseline alignment-baseline baseline-shift letter-spacing word-spacing text-decoration
	stop-color stop-opacity flood-color flood-opacity lighting-color marker-start marker-mid marker-end transform transform-origin`)

const svgDecimalNumberSyntax = `[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?`

var svgFragmentID = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)
var svgDecimalNumber = regexp.MustCompile(`^` + svgDecimalNumberSyntax + `$`)
var svgViewBoxNumbers = regexp.MustCompile(`^[ \t\r\n]*` + svgDecimalNumberSyntax + `(?:(?:[ \t\r\n]+|[ \t\r\n]*,[ \t\r\n]*)` + svgDecimalNumberSyntax + `){3}[ \t\r\n]*$`)
var svgCSSSelector = regexp.MustCompile(`^[A-Za-z0-9_.*#, >\t\r\n-]+$`)
var svgXMLDeclaration = regexp.MustCompile(`^version\s*=\s*(?:"1\.0"|'1\.0')(?:\s+encoding\s*=\s*(?:"[Uu][Tt][Ff]-8"|'[Uu][Tt][Ff]-8'))?(?:\s+standalone\s*=\s*(?:"(?:yes|no)"|'(?:yes|no)'))?\s*$`)

type svgOutputNode struct {
	children []int
	refs     []string
}

func validateSVGOutput(data []byte, maxPixels int64) (ImageBytes, error) {
	decoder := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	var nodes []svgOutputNode
	var stack []int
	ids := make(map[string]int)
	var rootAttrs []xml.Attr
	rootClosed, declarationSeen := false, false
	styleNode := -1
	var styleText strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ImageBytes{}, newImageValidationError("invalid SVG XML")
		}
		switch token := token.(type) {
		case xml.Directive:
			return ImageBytes{}, newImageValidationError("SVG declarations and entities are not supported")
		case xml.ProcInst:
			if token.Target != "xml" || len(nodes) != 0 || declarationSeen || !svgXMLDeclaration.Match(bytes.TrimSpace(token.Inst)) {
				return ImageBytes{}, newImageValidationError("SVG processing instructions are not supported")
			}
			declarationSeen = true
		case xml.StartElement:
			if rootClosed || token.Name.Space != svgNamespace || !slices.Contains(svgOutputElements, token.Name.Local) || styleNode >= 0 {
				return ImageBytes{}, newImageValidationError("unsupported SVG element or namespace")
			}
			if len(nodes) >= 10000 || len(stack) >= 64 || len(token.Attr) > 64 {
				return ImageBytes{}, newImageValidationError("SVG structure limit exceeded")
			}
			if len(nodes) == 0 {
				if token.Name.Local != "svg" {
					return ImageBytes{}, newImageValidationError("invalid SVG root")
				}
				rootAttrs = token.Attr
			}
			index := len(nodes)
			nodes = append(nodes, svgOutputNode{})
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				nodes[parent].children = append(nodes[parent].children, index)
			}
			stack = append(stack, index)
			seenAttrs := make(map[xml.Name]bool, len(token.Attr))
			for _, attr := range token.Attr {
				if seenAttrs[attr.Name] || len(attr.Value) > 1<<20 {
					return ImageBytes{}, newImageValidationError("invalid or oversized SVG attribute")
				}
				seenAttrs[attr.Name] = true
				if attr.Name.Space == "xmlns" || attr.Name.Space == "" && attr.Name.Local == "xmlns" {
					if attr.Value != svgNamespace && attr.Value != "http://www.w3.org/1999/xlink" {
						return ImageBytes{}, newImageValidationError("unsupported SVG namespace declaration")
					}
					continue
				}
				if attr.Name.Space == "http://www.w3.org/XML/1998/namespace" && (attr.Name.Local == "space" || attr.Name.Local == "lang") {
					continue
				}
				if attr.Name.Local == "href" && (attr.Name.Space == "" || attr.Name.Space == "http://www.w3.org/1999/xlink") {
					if !slices.Contains([]string{"use", "textPath", "linearGradient", "radialGradient", "pattern"}, token.Name.Local) || !strings.HasPrefix(attr.Value, "#") || !svgFragmentID.MatchString(attr.Value[1:]) {
						return ImageBytes{}, newImageValidationError("SVG references must be local fragments")
					}
					nodes[index].refs = append(nodes[index].refs, attr.Value[1:])
					continue
				}
				if attr.Name.Space != "" {
					return ImageBytes{}, newImageValidationError("unsupported SVG attribute namespace")
				}
				if attr.Name.Local == "style" {
					refs, err := validateSVGStyleDeclarations(attr.Value)
					if err != nil {
						return ImageBytes{}, err
					}
					nodes[index].refs = append(nodes[index].refs, refs...)
					continue
				}
				if !slices.Contains(svgOutputAttributes, attr.Name.Local) {
					return ImageBytes{}, newImageValidationError("unsupported SVG attribute")
				}
				if attr.Name.Local == "id" {
					if !svgFragmentID.MatchString(attr.Value) {
						return ImageBytes{}, newImageValidationError("invalid SVG element ID")
					}
					if _, exists := ids[attr.Value]; exists {
						return ImageBytes{}, newImageValidationError("duplicate SVG element ID")
					}
					ids[attr.Value] = index
				}
				if slices.Contains([]string{"id", "class", "role", "aria-label", "aria-labelledby", "aria-describedby"}, attr.Name.Local) {
					continue
				}
				refs, err := validateSVGPresentationValue(attr.Value)
				if err != nil {
					return ImageBytes{}, err
				}
				nodes[index].refs = append(nodes[index].refs, refs...)
			}
			if token.Name.Local == "style" {
				styleNode = index
				styleText.Reset()
			}
		case xml.EndElement:
			if styleNode >= 0 {
				remaining := strings.TrimSpace(styleText.String())
				for remaining != "" {
					selector, body, ok := strings.Cut(remaining, "{")
					if !ok || strings.TrimSpace(selector) == "" || !svgCSSSelector.MatchString(selector) {
						return ImageBytes{}, newImageValidationError("unsupported SVG CSS selector")
					}
					declarations, rest, ok := strings.Cut(body, "}")
					if !ok {
						return ImageBytes{}, newImageValidationError("invalid SVG CSS rule")
					}
					refs, err := validateSVGStyleDeclarations(declarations)
					if err != nil {
						return ImageBytes{}, err
					}
					nodes[styleNode].refs = append(nodes[styleNode].refs, refs...)
					remaining = strings.TrimSpace(rest)
				}
				styleNode = -1
			}
			stack = stack[:len(stack)-1]
			rootClosed = len(stack) == 0
		case xml.CharData:
			if len(stack) == 0 && len(bytes.TrimSpace(token)) != 0 {
				return ImageBytes{}, newImageValidationError("unexpected data outside SVG root")
			}
			if styleNode >= 0 {
				if styleText.Len()+len(token) > 128<<10 {
					return ImageBytes{}, newImageValidationError("SVG stylesheet limit exceeded")
				}
				styleText.Write(token)
			}
		}
	}
	if len(nodes) == 0 || !rootClosed || len(stack) != 0 {
		return ImageBytes{}, newImageValidationError("incomplete SVG document")
	}
	width, height, err := svgOutputDimensions(rootAttrs)
	if err != nil {
		return ImageBytes{}, err
	}
	width, height = math.Ceil(width), math.Ceil(height)
	if maxPixels <= 0 || width > math.MaxInt32 || height > math.MaxInt32 || height > float64(maxPixels) || width > float64(maxPixels)/height {
		return ImageBytes{}, newImageValidationError("image pixel limit exceeded")
	}
	for i := range nodes {
		for _, ref := range nodes[i].refs {
			target, exists := ids[ref]
			if !exists {
				return ImageBytes{}, newImageValidationError("SVG reference target is missing")
			}
			nodes[i].children = append(nodes[i].children, target)
		}
	}
	if _, err := svgOutputExpansion(nodes, 0, make([]uint8, len(nodes)), make([]int, len(nodes))); err != nil {
		return ImageBytes{}, err
	}
	sum := sha256.Sum256(data)
	return ImageBytes{Data: data, ContentType: "image/svg+xml", Checksum: hex.EncodeToString(sum[:]), Width: int(width), Height: int(height)}, nil
}

func validateSVGStyleDeclarations(style string) ([]string, error) {
	var refs []string
	for declaration := range strings.SplitSeq(style, ";") {
		if strings.TrimSpace(declaration) == "" {
			continue
		}
		name, value, ok := strings.Cut(declaration, ":")
		if !ok || !slices.Contains(svgOutputCSSProperties, strings.ToLower(strings.TrimSpace(name))) {
			return nil, newImageValidationError("unsupported SVG CSS property")
		}
		valueRefs, err := validateSVGPresentationValue(value)
		if err != nil {
			return nil, err
		}
		refs = append(refs, valueRefs...)
	}
	return refs, nil
}

// Reject CSS escapes and comments rather than attempting to normalize a second
// language. All resource functions must be literal local fragment references.
func validateSVGPresentationValue(value string) ([]string, error) {
	if strings.ContainsAny(value, "\\@<>{}") || strings.Contains(value, "/*") || strings.Contains(value, "*/") {
		return nil, newImageValidationError("unsupported SVG presentation value")
	}
	for _, char := range value {
		if char < 0x20 && char != '\t' && char != '\r' && char != '\n' || char == 0x7f {
			return nil, newImageValidationError("invalid SVG presentation character")
		}
	}
	var refs []string
	remaining := value
	for {
		before, after, found := strings.Cut(remaining, "(")
		if !found {
			break
		}
		start := len(before)
		for start > 0 && (before[start-1] >= 'a' && before[start-1] <= 'z' || before[start-1] >= 'A' && before[start-1] <= 'Z') {
			start--
		}
		function := strings.ToLower(before[start:])
		if !slices.Contains([]string{"url", "rgb", "rgba", "hsl", "hsla", "matrix", "translate", "translatex", "translatey", "scale", "scalex", "scaley", "rotate", "skewx", "skewy"}, function) {
			return nil, newImageValidationError("unsupported SVG presentation function")
		}
		argument, rest, ok := strings.Cut(after, ")")
		if !ok || strings.Contains(argument, "(") {
			return nil, newImageValidationError("invalid SVG presentation function")
		}
		if function == "url" {
			fragment := strings.TrimSpace(argument)
			if len(fragment) >= 2 && (fragment[0] == '\'' && fragment[len(fragment)-1] == '\'' || fragment[0] == '"' && fragment[len(fragment)-1] == '"') {
				fragment = fragment[1 : len(fragment)-1]
			}
			if !strings.HasPrefix(fragment, "#") || !svgFragmentID.MatchString(fragment[1:]) {
				return nil, newImageValidationError("SVG resources must be local fragments")
			}
			refs = append(refs, fragment[1:])
		}
		remaining = rest
	}
	return refs, nil
}

func svgOutputDimensions(attrs []xml.Attr) (float64, float64, error) {
	var rawWidth, rawHeight, viewBox string
	for _, attr := range attrs {
		if attr.Name.Space != "" {
			continue
		}
		switch attr.Name.Local {
		case "width":
			rawWidth = attr.Value
		case "height":
			rawHeight = attr.Value
		case "viewBox":
			viewBox = attr.Value
		}
	}
	var viewWidth, viewHeight float64
	if viewBox != "" {
		// FieldsFunc alone would discard empty comma-separated components and
		// accept a viewBox which browsers ignore as malformed.
		if !svgViewBoxNumbers.MatchString(viewBox) {
			return 0, 0, newImageValidationError("invalid SVG viewBox")
		}
		parts := strings.FieldsFunc(viewBox, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\r' || r == '\n' })
		if len(parts) != 4 {
			return 0, 0, newImageValidationError("invalid SVG viewBox")
		}
		values := make([]float64, 4)
		for i, part := range parts {
			if !svgDecimalNumber.MatchString(part) {
				return 0, 0, newImageValidationError("invalid SVG viewBox")
			}
			value, err := strconv.ParseFloat(part, 64)
			if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
				return 0, 0, newImageValidationError("invalid SVG viewBox")
			}
			values[i] = value
		}
		viewWidth, viewHeight = values[2], values[3]
		if viewWidth <= 0 || viewHeight <= 0 {
			return 0, 0, newImageValidationError("invalid SVG viewBox")
		}
	}
	width, err := svgOutputLength(rawWidth, viewWidth)
	if err != nil {
		return 0, 0, err
	}
	height, err := svgOutputLength(rawHeight, viewHeight)
	if err != nil {
		return 0, 0, err
	}
	if rawWidth == "" && rawHeight != "" && viewHeight > 0 {
		width = height * viewWidth / viewHeight
	}
	if rawHeight == "" && rawWidth != "" && viewWidth > 0 {
		height = width * viewHeight / viewWidth
	}
	if width <= 0 || height <= 0 || math.IsInf(width, 0) || math.IsInf(height, 0) || math.IsNaN(width) || math.IsNaN(height) {
		return 0, 0, newImageValidationError("SVG requires positive dimensions or a viewBox")
	}
	return width, height, nil
}

func svgOutputLength(raw string, fallback float64) (float64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "100%" {
		return fallback, nil
	}
	factor := 1.0
	for unit, ratio := range map[string]float64{"px": 1, "pt": 96.0 / 72, "pc": 16, "in": 96, "cm": 96.0 / 2.54, "mm": 96.0 / 25.4, "Q": 96.0 / 101.6} {
		if number, found := strings.CutSuffix(raw, unit); found {
			raw, factor = number, ratio
			break
		}
	}
	if !svgDecimalNumber.MatchString(raw) {
		return 0, newImageValidationError("invalid SVG dimension")
	}
	value, err := strconv.ParseFloat(raw, 64)
	value *= factor
	if err != nil || value <= 0 || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, newImageValidationError("invalid SVG dimension")
	}
	return value, nil
}

// Containment and fragment references form a graph. Reject recursive references
// and cap expanded nodes before a browser can instantiate an exponential tree.
func svgOutputExpansion(nodes []svgOutputNode, index int, state []uint8, sizes []int) (int, error) {
	if state[index] == 1 {
		return 0, newImageValidationError("cyclic SVG references are not supported")
	}
	if state[index] == 2 {
		return sizes[index], nil
	}
	state[index] = 1
	size := 1
	for _, child := range nodes[index].children {
		childSize, err := svgOutputExpansion(nodes, child, state, sizes)
		if err != nil {
			return 0, err
		}
		size += childSize
		if size > 100000 {
			return 0, newImageValidationError("SVG reference expansion limit exceeded")
		}
	}
	state[index], sizes[index] = 2, size
	return size, nil
}
