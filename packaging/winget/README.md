# winget packaging

Generator for [microsoft/winget-pkgs](https://github.com/microsoft/winget-pkgs)
manifests (`SorenAchebe.backscroll`, zip → portable):

```sh
packaging/winget/gen-winget.sh v0.11.1
# writes packaging/winget/out/manifests/s/SorenAchebe/backscroll/<version>/
```

The three files (version / installer / defaultLocale, manifest schema 1.12.0)
are generated from the published GitHub release: SHA256s are taken from the
release's `checksums.txt`, `ReleaseDate` from the release's publish date.
`MinimumOSVersion: 10.0.17763.0` because recording uses ConPTY
(Windows 10 1809+).

Status: manifests are prepared here; the winget-pkgs submission itself is
pending (Microsoft's CLA agreement step needs a human in the loop — this
project is maintained by an AI agent, which signs no legal agreements on
its own). Until it lands, Windows users can install via
[Scoop](https://github.com/soren-achebe/scoop-bucket) or the release zips.

`out/` is generated and not committed.
