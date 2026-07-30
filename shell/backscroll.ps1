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

# Ctrl-X Ctrl-P: fuzzy-pick a past command (with output preview) and insert
# it at the prompt. Active in and out of recorded sessions; set
# BACKSCROLL_NO_BIND=1 before loading to skip. Needs fzf and PSReadLine.
if (-not $env:BACKSCROLL_NO_BIND -and (Get-Module PSReadLine)) {
    Set-PSReadLineKeyHandler -Chord 'Ctrl+x,Ctrl+p' `
        -BriefDescription 'backscrollPick' `
        -Description 'backscroll: fuzzy-pick a past command (with output preview) and insert it' `
        -ScriptBlock {
        $line = ''
        $cursor = 0
        [Microsoft.PowerShell.PSConsoleReadLine]::GetBufferState(
            [ref]$line, [ref]$cursor)
        # Inside a key handler PowerShell pipes a native command's stderr,
        # so fzf's UI (drawn on stderr) would be INVISIBLE while its
        # keystrokes still land — `& backscroll pick` looks like a hang.
        # Start the process directly with only stdout redirected: stderr
        # stays on the real terminal and the UI renders normally.
        $psi = [System.Diagnostics.ProcessStartInfo]::new('backscroll')
        $psi.RedirectStandardOutput = $true
        $psi.UseShellExecute = $false
        if ($PSVersionTable.PSVersion.Major -ge 6) {
            foreach ($a in @('pick', '--print-cmd', '--', "$line")) {
                [void]$psi.ArgumentList.Add($a)
            }
        } else {
            # .NET Framework (Windows PowerShell 5.1) has no ArgumentList;
            # compose the argument string with MSVCRT-style quoting.
            $q = ($line -replace '(\\*)"', '$1$1\"') -replace '(\\+)$', '$1$1'
            $psi.Arguments = 'pick --print-cmd -- "' + $q + '"'
        }
        try {
            $p = [System.Diagnostics.Process]::Start($psi)
        } catch {
            return  # backscroll not on PATH; nothing sensible to do
        }
        $sel = $p.StandardOutput.ReadToEnd().TrimEnd("`r", "`n")
        $p.WaitForExit()
        # fzf has scribbled over the prompt row; redraw, then replace the
        # buffer with the pick (cancelled pick = just redraw).
        [Microsoft.PowerShell.PSConsoleReadLine]::InvokePrompt()
        if ($sel) {
            [Microsoft.PowerShell.PSConsoleReadLine]::RevertLine()
            [Microsoft.PowerShell.PSConsoleReadLine]::Insert($sel)
        }
    }
}

# Tab completion for backscroll subcommands and flags.
# Active whenever backscroll is on PATH; no env var needed.
Register-ArgumentCompleter -Native -CommandName backscroll -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)

    $cmdline = $commandAst.CommandElements
    $sub = if ($cmdline.Count -ge 2) { $cmdline[1].Value } else { '' }

    $subs = 'run exec init list last show search pick diff export import sync stats note prune delete redact mcp serve off on doctor upgrade version help'
    $initTargets = 'bash zsh fish pwsh tmux zellij screen'
    $exportFormats = 'md cast json html'
    $importSources = 'atuin zsh bash fish nu pwsh'
    $syncSubs = 'init export import status'
    $byValues = 'cmd cwd exit host session day'
    $exitValues = 'fail 0 1 2'

    if ($cmdline.Count -eq 2 -or ($cmdline.Count -eq 1 -and $wordToComplete)) {
        $subs.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
            ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        return
    }

    switch ($sub) {
        'init' {
            if ($cmdline.Count -eq 3 -or ($cmdline.Count -eq 2 -and $wordToComplete)) {
                $initTargets.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
        'export' {
            $flags = '--format --details --raw --redact -o -n --session --cwd --exit --since --until --host'
            if ($wordToComplete -eq '--format' -or $wordToComplete -eq '-format') {
                $exportFormats.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            } elseif ($wordToComplete.StartsWith('-')) {
                $flags.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            } elseif ($wordToComplete -eq '--exit' -or $wordToComplete -eq '-exit') {
                $exitValues.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
        'exec' {
            if ($wordToComplete.StartsWith('-')) {
                '--quiet --head-cap --tail-cap'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
        'import' {
            if ($cmdline.Count -eq 3 -or ($cmdline.Count -eq 2 -and $wordToComplete)) {
                $importSources.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            } else {
                '--dry-run --host -n'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
        'sync' {
            if ($cmdline.Count -eq 3 -or ($cmdline.Count -eq 2 -and $wordToComplete)) {
                $syncSubs.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
        'note' {
            if ($wordToComplete.StartsWith('-')) {
                '--rm'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
        'prune' {
            '--older --max-size'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'doctor' {
            '--reindex'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'upgrade' {
            '--check --version'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        'serve' {
            '--addr --redact --open'.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
        }
        { $_ -in 'list', 'last', 'search', 'pick', 'stats' } {
            if ($wordToComplete.StartsWith('-')) {
                $flags = '-n --session --cwd --exit --since --until --host'
                if ($sub -eq 'pick') { $flags += ' --pager --raw --print-id --print-cmd --redact' }
                if ($sub -eq 'search') { $flags += ' -A -B -C --redact' }
                if ($sub -eq 'stats') { $flags += ' --by' }
                $flags.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            } elseif ($wordToComplete -eq '--by' -or $wordToComplete -eq '-by') {
                $byValues.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            } elseif ($wordToComplete -eq '--exit' -or $wordToComplete -eq '-exit') {
                $exitValues.Split(' ') | Where-Object { $_ -like "$wordToComplete*" } |
                    ForEach-Object { [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_) }
            }
        }
    }
}
