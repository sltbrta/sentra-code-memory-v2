package codeindex

func classifyTokens(
	language Language,
	tokens []sourceToken,
	coverage Coverage,
) []classifiedToken {
	classified := make([]Kind, len(tokens))
	if coverage == CoverageSyntaxAware {
		markImports(language, tokens, classified)
		markDefinitions(language, tokens, classified)
	}

	result := make([]classifiedToken, 0, len(tokens))
	for index, token := range tokens {
		kind := classified[index]
		if kind == "" {
			if token.kind != tokenIdentifier || isKeyword(language, token.text) {
				continue
			}
			kind = KindReference
		}
		result = append(result, classifiedToken{kind: kind, token: token})
	}
	return result
}

type classifiedToken struct {
	kind  Kind
	token sourceToken
}

func markDefinitions(language Language, tokens []sourceToken, kinds []Kind) {
	switch language {
	case LanguageGo:
		markFollowingDefinitions(language, tokens, kinds, map[string]bool{
			"type": true, "var": true, "const": true,
		})
		markGoFunctions(tokens, kinds)
	case LanguageTypeScript:
		markFollowingDefinitions(language, tokens, kinds, map[string]bool{
			"function": true, "class": true, "interface": true, "type": true,
			"enum": true, "namespace": true, "const": true, "let": true, "var": true,
		})
	case LanguagePython:
		markFollowingDefinitions(language, tokens, kinds, map[string]bool{"def": true, "class": true})
	case LanguageRust:
		markFollowingDefinitions(language, tokens, kinds, map[string]bool{
			"fn": true, "struct": true, "enum": true, "trait": true, "type": true,
			"mod": true, "const": true, "static": true, "let": true,
		})
	case LanguageJava:
		markFollowingDefinitions(language, tokens, kinds, map[string]bool{
			"class": true, "interface": true, "enum": true, "record": true,
		})
		markJavaMethods(tokens, kinds)
	}
}

func markFollowingDefinitions(
	language Language,
	tokens []sourceToken,
	kinds []Kind,
	starters map[string]bool,
) {
	for index, token := range tokens {
		if token.kind != tokenIdentifier || !starters[token.text] || kinds[index] != "" {
			continue
		}
		if language == LanguageTypeScript && isTypeScriptVariableStarter(token.text) {
			next := nextSignificant(tokens, index+1)
			if next >= 0 && (tokens[next].text == "{" || tokens[next].text == "[") {
				continue
			}
		}
		next := nextNonKeywordIdentifier(language, tokens, index+1)
		if next >= 0 && kinds[next] == "" {
			kinds[next] = KindDefinition
		}
	}
}

func isTypeScriptVariableStarter(text string) bool {
	return text == "const" || text == "let" || text == "var"
}

func markGoFunctions(tokens []sourceToken, kinds []Kind) {
	for index, token := range tokens {
		if token.kind != tokenIdentifier || token.text != "func" || kinds[index] != "" {
			continue
		}
		cursor := nextSignificant(tokens, index+1)
		if cursor < 0 {
			continue
		}
		if tokens[cursor].text == "(" {
			cursor = afterMatching(tokens, cursor, "(", ")")
		}
		cursor = nextIdentifier(tokens, cursor)
		if cursor >= 0 && kinds[cursor] == "" {
			kinds[cursor] = KindDefinition
		}
	}
}

func markJavaMethods(tokens []sourceToken, kinds []Kind) {
	control := map[string]bool{
		"if": true, "for": true, "while": true, "switch": true, "catch": true,
		"new": true, "return": true, "throw": true,
	}
	for index := 1; index+1 < len(tokens); index++ {
		if tokens[index].kind != tokenIdentifier || tokens[index+1].text != "(" || kinds[index] != "" {
			continue
		}
		previous := previousSignificant(tokens, index-1)
		if previous < 0 || tokens[previous].kind != tokenIdentifier || control[tokens[previous].text] {
			continue
		}
		kinds[index] = KindDefinition
	}
}

func markImports(language Language, tokens []sourceToken, kinds []Kind) {
	for index, token := range tokens {
		if token.kind != tokenIdentifier || !isImportStarter(language, token.text) {
			continue
		}
		end := statementEnd(tokens, index+1, language)
		for cursor := index + 1; cursor < end; cursor++ {
			candidate := tokens[cursor]
			if candidate.kind == tokenString {
				kinds[cursor] = KindImport
				continue
			}
			if candidate.kind == tokenIdentifier && !isKeyword(language, candidate.text) {
				kinds[cursor] = KindImport
			}
		}
	}
}

func isImportStarter(language Language, text string) bool {
	switch language {
	case LanguageGo, LanguageTypeScript, LanguageJava:
		return text == "import"
	case LanguagePython:
		return text == "import" || text == "from"
	case LanguageRust:
		return text == "use"
	default:
		return false
	}
}

func statementEnd(tokens []sourceToken, start int, language Language) int {
	depth := 0
	seenString := false
	for index := start; index < len(tokens); index++ {
		current := tokens[index]
		if current.kind == tokenString {
			seenString = true
		}
		switch current.text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			if depth > 0 {
				depth--
			}
		}
		if current.text == ";" && depth == 0 {
			return index
		}
		if current.kind != tokenNewline || depth != 0 {
			continue
		}
		if language == LanguageGo ||
			(language == LanguagePython && !explicitLineContinuation(tokens, index)) ||
			(language == LanguageTypeScript && seenString) {
			return index
		}
	}
	return len(tokens)
}

func explicitLineContinuation(tokens []sourceToken, newline int) bool {
	previous := previousSignificant(tokens, newline-1)
	return previous >= 0 && tokens[previous].text == "\\"
}

func nextNonKeywordIdentifier(language Language, tokens []sourceToken, start int) int {
	for index := max(start, 0); index < len(tokens); index++ {
		if tokens[index].kind == tokenIdentifier && !isKeyword(language, tokens[index].text) {
			return index
		}
		if tokens[index].kind == tokenNewline || tokens[index].text == ";" {
			return -1
		}
	}
	return -1
}

func nextIdentifier(tokens []sourceToken, start int) int {
	for index := max(start, 0); index < len(tokens); index++ {
		if tokens[index].kind == tokenIdentifier {
			return index
		}
		if tokens[index].kind == tokenNewline || tokens[index].text == ";" {
			return -1
		}
	}
	return -1
}

func nextSignificant(tokens []sourceToken, start int) int {
	for index := max(start, 0); index < len(tokens); index++ {
		if tokens[index].kind != tokenNewline {
			return index
		}
	}
	return -1
}

func previousSignificant(tokens []sourceToken, start int) int {
	for index := start; index >= 0; index-- {
		if tokens[index].kind != tokenNewline {
			return index
		}
	}
	return -1
}

func afterMatching(tokens []sourceToken, start int, open, close string) int {
	depth := 0
	for index := start; index < len(tokens); index++ {
		switch tokens[index].text {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return index + 1
			}
		}
	}
	return -1
}

func isKeyword(language Language, text string) bool {
	return languageKeywords[language][text]
}

var languageKeywords = map[Language]map[string]bool{
	LanguageGo: keywordSet(
		"break", "default", "func", "interface", "select", "case", "defer", "go", "map",
		"struct", "chan", "else", "goto", "package", "switch", "const", "fallthrough", "if",
		"range", "type", "continue", "for", "import", "return", "var",
	),
	LanguageTypeScript: keywordSet(
		"as", "async", "await", "break", "case", "catch", "class", "const", "continue",
		"default", "delete", "do", "else", "enum", "export", "extends", "false", "finally",
		"for", "from", "function", "if", "implements", "import", "in", "instanceof", "interface",
		"let", "namespace", "new", "null", "of", "private", "protected", "public", "return",
		"static", "super", "switch", "this", "throw", "true", "try", "type", "typeof", "undefined",
		"var", "void", "while", "with", "yield",
	),
	LanguagePython: keywordSet(
		"and", "as", "assert", "async", "await", "break", "class", "continue", "def", "del",
		"elif", "else", "except", "False", "finally", "for", "from", "global", "if", "import",
		"in", "is", "lambda", "None", "nonlocal", "not", "or", "pass", "raise", "return", "True",
		"try", "while", "with", "yield",
	),
	LanguageRust: keywordSet(
		"as", "async", "await", "break", "const", "continue", "crate", "dyn", "else", "enum",
		"extern", "false", "fn", "for", "if", "impl", "in", "let", "loop", "match", "mod",
		"move", "mut", "pub", "ref", "return", "self", "Self", "static", "struct", "super",
		"trait", "true", "type", "unsafe", "use", "where", "while",
	),
	LanguageJava: keywordSet(
		"abstract", "assert", "boolean", "break", "byte", "case", "catch", "char", "class",
		"const", "continue", "default", "do", "double", "else", "enum", "extends", "false",
		"final", "finally", "float", "for", "goto", "if", "implements", "import", "instanceof",
		"int", "interface", "long", "native", "new", "null", "package", "private", "protected",
		"public", "record", "return", "short", "static", "strictfp", "super", "switch",
		"synchronized", "this", "throw", "throws", "transient", "true", "try", "void", "volatile", "while",
	),
}

func keywordSet(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}
