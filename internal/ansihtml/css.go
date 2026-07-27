package ansihtml

// Palette defines the CSS custom properties (--c0..--c15) used by the
// class rules in CSS. GitHub-dark-ish 16-color palette; hosts may
// override the variables to retheme without touching the rules.
const Palette = `--c0:#484f58;--c1:#ff7b72;--c2:#3fb950;--c3:#d29922;--c4:#58a6ff;--c5:#bc8cff;--c6:#39c5cf;--c7:#b1bac4;
--c8:#6e7681;--c9:#ffa198;--c10:#56d364;--c11:#e3b341;--c12:#79c0ff;--c13:#d2a8ff;--c14:#56d4dd;--c15:#f0f6fc;`

// CSS contains the class rules for every class Render emits. Embed it
// (plus Palette inside a selector that wraps the output, e.g. :root)
// in any page that shows Render's HTML. web/index.html carries a copy
// inline so it stays a single self-contained file; a test pins the two
// in sync.
const CSS = `.f0{color:var(--c0)} .f1{color:var(--c1)} .f2{color:var(--c2)} .f3{color:var(--c3)}
.f4{color:var(--c4)} .f5{color:var(--c5)} .f6{color:var(--c6)} .f7{color:var(--c7)}
.f8{color:var(--c8)} .f9{color:var(--c9)} .f10{color:var(--c10)} .f11{color:var(--c11)}
.f12{color:var(--c12)} .f13{color:var(--c13)} .f14{color:var(--c14)} .f15{color:var(--c15)}
.b0{background:var(--c0)} .b1{background:var(--c1)} .b2{background:var(--c2)} .b3{background:var(--c3)}
.b4{background:var(--c4)} .b5{background:var(--c5)} .b6{background:var(--c6)} .b7{background:var(--c7)}
.b8{background:var(--c8)} .b9{background:var(--c9)} .b10{background:var(--c10)} .b11{background:var(--c11)}
.b12{background:var(--c12)} .b13{background:var(--c13)} .b14{background:var(--c14)} .b15{background:var(--c15)}
.bo{font-weight:700} .di{opacity:.6} .it{font-style:italic}
.ul{text-decoration:underline} .st{text-decoration:line-through}
.rv{background:var(--fg);color:var(--bg)}`
