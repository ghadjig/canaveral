import Quickshell
import Quickshell.Io
import Quickshell.Wayland
import Quickshell.Hyprland
import QtQuick

// The start-from-anywhere launcher: a popup that turns a few typed characters
// into a canaveral command against any registered project.
//
// The grammar is `<project> [command] [args...]`, and everything after the
// project is an ordinary canaveral argv — so a line here maps onto
// `canaveral -C <project> <argv>` with no translation, and one completion
// engine serves both. Nothing about that grammar is implemented here: this file
// draws a text box, shells out to `canaveral complete --launcher`, and renders
// the JSON it gets back. If the completions are wrong, the bug is in Go.
//
// Shown and hidden over IPC rather than by spawning a process per keypress:
//
//     qs -c canaveral ipc call launcher toggle
//
// The bar is already resident, so the popup costs nothing to keep around and
// opens instantly, which a launcher has to.
PanelWindow {
    id: root

    // ---- geometry ----
    readonly property int cardWidth: 720
    readonly property int maxRows: 8
    readonly property int rowH: 30
    readonly property int pad: 16

    property bool open: false
    // A line the user has to confirm before it runs. Set for commands canaveral
    // marks destructive: a mistyped `rm` costs far more from a global hotkey
    // than from a shell, where at least you saw yourself type it.
    property bool confirming: false

    property var result: null
    property int selected: 0

    // Resolved once rather than assumed: Hyprland keybinds inherit the
    // session's PATH, which does not always include ~/.local/bin, and this
    // shell was started from that same session. The same trap canaveral-goto
    // documents.
    property string bin: "canaveral"

    visible: open
    color: "transparent"
    // Fullscreen so a click anywhere outside the card dismisses it. Nothing is
    // drawn outside the card except the backdrop, and the window does not exist
    // at all while closed.
    anchors {
        top: true
        bottom: true
        left: true
        right: true
    }
    WlrLayershell.layer: WlrLayer.Overlay
    WlrLayershell.namespace: "canaveral-launcher"
    WlrLayershell.exclusionMode: ExclusionMode.Ignore
    // Exclusive only while open: an overlay holding the keyboard permanently
    // would make the rest of the session untypeable.
    WlrLayershell.keyboardFocus: open ? WlrKeyboardFocus.Exclusive : WlrKeyboardFocus.None
    focusable: open

    // ---- lifecycle ----

    function toggle() {
        if (open)
            hide();
        else
            show();
    }

    function show() {
        // Open on the monitor being looked at, not on whichever screen happens
        // to be first in the list.
        const want = Hyprland.focusedMonitor ? Hyprland.focusedMonitor.name : "";
        for (const s of Quickshell.screens) {
            if (s.name === want) {
                root.screen = s;
                break;
            }
        }
        input.text = "";
        result = null;
        selected = 0;
        confirming = false;
        open = true;
        input.forceActiveFocus();
        refresh();
    }

    function hide() {
        open = false;
        debounce.stop();
    }

    IpcHandler {
        target: "launcher"

        function toggle(): void {
            root.toggle();
        }
        // Not called "show": `qs ipc` has its own `show` subcommand, and
        // `qs ipc call launcher show` is swallowed by it — the listing prints
        // and the function never runs. Confirmed on 0.3.0.
        function popup(): void {
            root.show();
        }
        function hide(): void {
            root.hide();
        }
    }

    Process {
        running: true
        command: ["sh", "-c", "command -v canaveral || echo \"$HOME/.local/bin/canaveral\""]
        stdout: StdioCollector {
            onStreamFinished: {
                const p = this.text.trim();
                if (p)
                    root.bin = p;
            }
        }
    }

    // ---- the line, as words ----

    // Split the way bash's COMP_WORDS does, keeping a trailing empty word when
    // the line ends in a space: that empty word is what says "completing a
    // fresh argument" rather than "still typing the last one".
    function words() {
        const parts = input.text.split(" ");
        const out = [];
        for (let i = 0; i < parts.length; i++) {
            if (parts[i] !== "" || i === parts.length - 1)
                out.push(parts[i]);
        }
        return out.length ? out : [""];
    }

    function lastWord() {
        const w = words();
        return w[w.length - 1];
    }

    function replaceLastWord(value) {
        const w = words();
        w[w.length - 1] = value;
        input.text = w.join(" ");
        input.cursorPosition = input.text.length;
    }

    // The argv Enter will run. Shown in the footer verbatim, because a launcher
    // that hides what it is about to do is the one you stop trusting.
    //
    // Empty until there is something to run: a project on its own is not a
    // command, and `canaveral -C norules` with nothing after it would exit 0
    // having printed usage to a terminal nobody is looking at.
    // The command actually executed, which is argv() behind an `env -u` that
    // strips canaveral's own feature variables.
    //
    // A popup has no "current feature", but canaveral does: since v0.3.0 `rm`
    // with no name falls back to whichever feature the shell belongs to, read
    // from those variables. Quickshell inherits its environment from whatever
    // launched it, so a bar restarted from inside a feature's terminal would
    // carry them, and a bare `rm` typed here would tear down a feature nobody
    // named.
    //
    // Done with `env` rather than Process.environment because assigning a JS
    // object to that property silently fails on quickshell 0.3.0 — it logs
    // "Unable to assign QJSValue to QVariantHash" and leaves the environment
    // untouched, which is exactly the failure mode a safety guard must not
    // have. `env` is a real binary with well-defined behaviour.
    function execArgv() {
        const a = argv();
        if (a.length === 0)
            return [];
        return ["env", "-u", "CANAVERAL_PROJECT", "-u", "CANAVERAL_FEATURE", "-u", "CANAVERAL_WORKTREE"].concat(a);
    }

    function argv() {
        const w = words().filter(x => x !== "");
        if (w.length < 2)
            return [];
        const rest = w.slice(1);
        const out = [root.bin, "-C", w[0]].concat(rest);
        // Neither `new` nor bare dispatch focuses the new workspace by default,
        // since both are normally run from a terminal you are already looking
        // at. From a launcher, going there is the entire intent.
        if (result && (result.command === "open" || result.command === "new") && rest.indexOf("--focus") < 0)
            out.push("--focus");
        return out;
    }

    readonly property var candidates: result && result.candidates ? result.candidates : []
    readonly property var currentCandidate: candidates.length > selected ? candidates[selected] : null

    // ---- completion ----

    Timer {
        id: debounce
        // Long enough to skip the intermediate states of a fast typist, short
        // enough to feel immediate. The completer is a Go binary reading a
        // handful of small files, so this is about not stacking processes, not
        // about the cost of any one of them.
        interval: 30
        onTriggered: root.fire()
    }

    property bool completionQueued: false

    function refresh() {
        debounce.restart();
    }

    function fire() {
        if (completer.running) {
            // A request that lands mid-flight is not dropped: the exit handler
            // reruns with whatever the line says by then, which is what the
            // user actually wants completed.
            completionQueued = true;
            return;
        }
        completionQueued = false;
        completer.command = [root.bin, "complete", "--launcher", "--"].concat(words());
        completer.running = true;
    }

    Process {
        id: completer
        running: false
        stdout: StdioCollector {
            onStreamFinished: {
                try {
                    root.result = JSON.parse(this.text);
                } catch (e) {
                    root.result = null;
                }
                root.selected = 0;
            }
        }
        onExited: if (root.completionQueued)
            root.fire()
    }

    // ---- running ----

    function accept(cand) {
        if (!cand)
            return;
        replaceLastWord(cand.value);
        // A namespace is a prefix of a longer answer, never an answer, so the
        // word stays open. Anything else is finished and the next argument
        // starts.
        if (!cand.continues)
            input.text += " ";
        input.cursorPosition = input.text.length;
        refresh();
    }

    // Enter completes before it runs: if the highlighted candidate merely
    // extends what has been typed, take it and wait. That makes Enter safe to
    // lean on — it can never run a half-typed name — while still running
    // immediately once the line says what it means.
    function activate() {
        const c = currentCandidate;
        const w = lastWord();
        // Only a word the user actually started typing can be disambiguated
        // this way. An empty trailing word means there is nothing to
        // complete — most often a space `accept()` itself just appended
        // after finishing the previous argument — and treating its
        // candidate (offered because some commands take a list, like
        // `reset [feature...]`) as something to silently accept would eat
        // an Enter that was meant to run an already-complete line, appending
        // an argument nobody asked for.
        if (w !== "") {
            if (c && c.value !== w) {
                accept(c);
                return;
            }
            if (c && c.continues)
                return;
        }
        if (result && result.destructive && !confirming) {
            confirming = true;
            return;
        }
        run();
    }

    // Submit and get out of the way.
    //
    // Bringing a feature up takes as long as its slowest readiness probe, and
    // sitting on a focused, still-typeable popup for that is the wrong place to
    // wait: the bar already has a row for the feature, and canaveral now
    // publishes its progress there. So the window closes at once and the work
    // is watched where everything else about that feature is watched.
    //
    // The process stays a child of this shell rather than being detached,
    // purely so its exit status is still knowable. Anything that fails before
    // the feature record exists — an unknown project, a name that is already
    // taken — has no row to fail on, and would otherwise vanish silently. Those
    // become a desktop notification instead.
    function run() {
        const cmd = execArgv();
        if (cmd.length === 0)
            return;
        confirming = false;
        lastCmd = argv().slice(1).join(" ");
        errorLines = [];
        // Fire-and-forget: the line is worth remembering even if the command
        // itself goes on to fail, since retyping it to try again is exactly
        // what history is for. Detached rather than tracked, because losing
        // this one entry to a race with the popup closing costs nothing.
        Quickshell.execDetached([root.bin, "complete", "--record", "--"].concat(words()));
        runner.command = cmd;
        runner.running = true;
        hide();
    }

    // The last command's stderr, kept only until it is reported.
    property var errorLines: []
    property string lastCmd: ""

    function notifyFailure(code) {
        const detail = errorLines.length ? errorLines.join("\n") : ("exited " + code);
        Quickshell.execDetached(["notify-send", "--app-name=canaveral", "--urgency=critical", "canaveral " + lastCmd, detail]);
    }

    Process {
        id: runner
        running: false
        // Survives the popup closing: quickshell is resident, so this keeps
        // running after hide(). If the bar itself is killed mid-flight the
        // reconcile dies with it, which is survivable by design — the feature
        // engine is idempotent and `canaveral reset` repairs a partial run.

        // stdout is the reporter's step-by-step prose, which the bar's own
        // progress row now says better and in the right place. Only stderr is
        // kept, and only to explain a failure.
        stderr: SplitParser {
            splitMarker: "\n"
            onRead: line => {
                if (line.trim())
                    root.errorLines = root.errorLines.concat([line]).slice(-6);
            }
        }
        onExited: code => {
            if (code !== 0)
                root.notifyFailure(code);
            root.errorLines = [];
        }
    }

    // ---- the surface ----

    MouseArea {
        anchors.fill: parent
        onClicked: root.hide()
    }

    Rectangle {
        anchors.fill: parent
        color: "#000000"
        opacity: 0.35
    }

    Rectangle {
        id: card

        anchors.horizontalCenter: parent.horizontalCenter
        y: Math.round(parent.height * 0.22)
        width: root.cardWidth
        height: content.implicitHeight + root.pad * 2
        radius: Theme.radius.card
        color: Theme.surface
        border.width: 1
        border.color: root.confirming ? Theme.status.failed : Theme.border

        // Swallows clicks so they do not reach the dismiss handler behind.
        MouseArea {
            anchors.fill: parent
        }

        Column {
            id: content
            anchors.fill: parent
            anchors.margins: root.pad
            spacing: 10

            // ---- prompt ----
            Row {
                width: parent.width
                spacing: 8

                Text {
                    text: "\uf0e7"
                    font.family: Theme.font.family
                    font.pixelSize: Theme.font.title
                    color: Theme.accent
                    anchors.verticalCenter: parent.verticalCenter
                }

                TextInput {
                    id: input

                    width: parent.width - 32
                    anchors.verticalCenter: parent.verticalCenter
                    font.family: Theme.font.family
                    font.pixelSize: Theme.font.title + 4
                    color: Theme.text
                    selectionColor: Theme.accentBorder
                    selectedTextColor: Theme.text
                    focus: true
                    activeFocusOnPress: true

                    onTextChanged: {
                        // Any edit invalidates a pending confirmation: the line
                        // being confirmed is no longer the line on screen.
                        root.confirming = false;
                        root.refresh();
                    }

                    Keys.onPressed: event => {
                        switch (event.key) {
                        case Qt.Key_Escape:
                            if (root.confirming)
                                root.confirming = false;
                            else
                                root.hide();
                            event.accepted = true;
                            break;
                        case Qt.Key_Tab:
                            // Tab takes the common prefix when several
                            // candidates share one, and the whole candidate
                            // when it is unambiguous — the completion key,
                            // never the run key.
                            //
                            // Guarded the same way Enter is: only a word the
                            // user actually started typing can be completed
                            // this way. An empty word means nothing was
                            // typed, and for a command that takes a list
                            // (`reset [feature...]`) there is always exactly
                            // one candidate to offer for "the next one" when
                            // a project has exactly one feature — so without
                            // this, completing the first argument leaves a
                            // trailing space, Tab "completes" that empty slot
                            // to the sole candidate, which itself completes
                            // to a single feature, and round it goes forever.
                            if (root.lastWord() !== "") {
                                if (root.candidates.length === 1)
                                    root.accept(root.candidates[0]);
                                else if (root.result && root.result.common !== root.lastWord())
                                    root.replaceLastWord(root.result.common);
                                else if (root.currentCandidate)
                                    root.accept(root.currentCandidate);
                            }
                            event.accepted = true;
                            break;
                        case Qt.Key_Down:
                            if (root.candidates.length)
                                root.selected = (root.selected + 1) % root.candidates.length;
                            event.accepted = true;
                            break;
                        case Qt.Key_Up:
                            if (root.candidates.length)
                                root.selected = (root.selected + root.candidates.length - 1) % root.candidates.length;
                            event.accepted = true;
                            break;
                        case Qt.Key_Return:
                        case Qt.Key_Enter:
                            root.activate();
                            event.accepted = true;
                            break;
                        }
                    }

                    Text {
                        anchors.verticalCenter: parent.verticalCenter
                        visible: input.text === ""
                        text: "project, then a command or a new feature name"
                        font: input.font
                        color: Theme.faint
                    }
                }
            }

            Rectangle {
                width: parent.width
                height: 1
                color: Theme.borderFaint
            }

            // ---- error from the completer ----
            Text {
                width: parent.width
                visible: !!(root.result && root.result.error)
                text: root.result && root.result.error ? root.result.error : ""
                wrapMode: Text.WordWrap
                font.family: Theme.font.family
                font.pixelSize: Theme.font.row
                color: Theme.status.waiting
            }

            // ---- candidates ----
            Column {
                id: list
                width: parent.width
                spacing: 1

                Repeater {
                    model: root.candidates.slice(0, root.maxRows)

                    Rectangle {
                        required property var modelData
                        required property int index

                        width: list.width
                        height: root.rowH
                        radius: Theme.radius.row
                        color: index === root.selected ? Theme.raised : "transparent"
                        border.width: 1
                        border.color: index === root.selected ? Theme.accentBorder : "transparent"

                        MouseArea {
                            anchors.fill: parent
                            hoverEnabled: true
                            onEntered: root.selected = index
                            onClicked: root.accept(modelData)
                        }

                        Row {
                            anchors.fill: parent
                            anchors.leftMargin: 10
                            anchors.rightMargin: 10
                            spacing: 8

                            Text {
                                anchors.verticalCenter: parent.verticalCenter
                                width: 74
                                text: modelData.kind
                                font.family: Theme.font.family
                                font.pixelSize: Theme.font.label
                                // "new" is the only candidate that creates
                                // something rather than naming something that
                                // exists, so it is the only one coloured.
                                color: modelData.kind === "new" ? Theme.accent : Theme.faint
                            }

                            Text {
                                anchors.verticalCenter: parent.verticalCenter
                                text: modelData.value
                                font.family: Theme.font.family
                                font.pixelSize: Theme.font.row + 1
                                color: index === root.selected ? Theme.text : Theme.dim
                            }

                            Text {
                                anchors.verticalCenter: parent.verticalCenter
                                text: modelData.desc || ""
                                font.family: Theme.font.family
                                font.pixelSize: Theme.font.detail
                                color: Theme.faint
                                elide: Text.ElideRight
                                width: Math.max(0, list.width - 120 - 90)
                            }
                        }
                    }
                }

                Text {
                    visible: root.candidates.length > root.maxRows
                    text: "+" + (root.candidates.length - root.maxRows) + " more"
                    font.family: Theme.font.family
                    font.pixelSize: Theme.font.label
                    color: Theme.faint
                    leftPadding: 10
                    topPadding: 4
                }
            }


            // ---- footer ----
            Text {
                width: parent.width
                font.family: Theme.font.family
                font.pixelSize: Theme.font.label
                elide: Text.ElideRight
                color: root.confirming ? Theme.status.failed : Theme.faint
                text: {
                    if (root.confirming)
                        return "this removes work — Enter to confirm, Esc to cancel";
                    const a = root.argv();
                    if (a.length === 0)
                        return "Tab completes · Enter runs · Esc closes";
                    if (root.result && root.result.fuzzy)
                        return "no exact match — " + a.join(" ");
                    return a.join(" ");
                }
            }
        }
    }
}
