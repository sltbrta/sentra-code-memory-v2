package scmfixture

// Beta references Alpha so route/impact verbs have a cross-file edge.
func Beta() int { return Alpha() + AlphaHelper() }
