# backscroll PowerShell integration — https://github.com/soren-achebe/backscroll
# Emits OSC 133 shell-integration marks (plus command text) so the
# backscroll recorder can segment output per command. No-op outside a
# backscroll session; safe to keep in $PROFILE permanently.
# Works on PowerShell 7+ (pwsh, all platforms) and Windows PowerShell 5.1.
#
#   Add to $PROFILE:   backscroll init pwsh | Out-String | Invoke-Expression

if ($env:BACKSCROLL_ACTIVE -and -not $env:BACKSCROLL_HOOKED) {
    $env:BACKSCROLL_HOOKED = '1'

    # `e is pwsh 6+ only; [char]27 keeps Windows PowerShell 5.1 working.
    $global:__bks_E = [char]27
    $global:__bks_B = [char]7

    # Capture the accepted command line: wrap PSConsoleHostReadLine, which
    # the console host calls for every input line. After PSReadLine returns
    # the line, report its text (base64, so no byte can break the OSC) and
    # mark the output start (133;C). Empty/blank accepts emit nothing — the
    # next prompt's unmatched 133;D is dropped by the recorder.
    if (Get-Module PSReadLine) {
        function global:PSConsoleHostReadLine {
            # PSReadLine ≥2.4 dropped the 2-arg ReadLine overload; older
            # ones lack the 3-arg. An unhandled MethodException here makes
            # the console host silently re-prompt forever, so try both.
            try {
                $line = [Microsoft.PowerShell.PSConsoleReadLine]::ReadLine(
                    $Host.Runspace, $ExecutionContext, $?)
            } catch [System.Management.Automation.MethodException] {
                $line = [Microsoft.PowerShell.PSConsoleReadLine]::ReadLine(
                    $Host.Runspace, $ExecutionContext)
            }
            if ($line -and $line.Trim()) {
                $b64 = [Convert]::ToBase64String(
                    [System.Text.Encoding]::UTF8.GetBytes($line))
                [Console]::Write("$global:__bks_E]6973;cmd=$b64$global:__bks_B$global:__bks_E]133;C$global:__bks_B")
            }
            $line
        }
    }

    # Wrap the prompt: close the previous command (133;D;exit), report the
    # cwd (OSC 7), open the new prompt (133;A), and mark its end (133;B).
    $global:__bks_origPrompt = $function:prompt
    function global:prompt {
        # First statement: capture last status before anything here
        # clobbers it. $? tracks native exit status too, so a stale
        # $LASTEXITCODE from an older native command is consulted only
        # when the command that just ran actually failed.
        $__bks_ok = $global:?
        $ec = 0
        if (-not $__bks_ok) {
            $ec = 1
            if ($global:LASTEXITCODE -is [int] -and $global:LASTEXITCODE -ne 0) {
                $ec = $global:LASTEXITCODE
            }
        }

        $p = $PWD.ProviderPath -replace '\\', '/'
        if (-not $p.StartsWith('/')) { $p = '/' + $p }
        $p = $p -replace '%', '%25' -replace ' ', '%20' -replace ';', '%3B'
        $mach = [System.Environment]::MachineName

        $body = if ($global:__bks_origPrompt) { & $global:__bks_origPrompt } else { "PS $PWD> " }
        $E = $global:__bks_E; $B = $global:__bks_B
        "$E]133;D;$ec$B$E]7;file://$mach$p$B$E]133;A$B$body$E]133;B$B"
    }
}
