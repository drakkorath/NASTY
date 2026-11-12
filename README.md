# NASTY? or (Not Another Setup Tool, Yes?)

**NASTY** is a small desktop app for NixOS. It lets you install and remove packages, flip a few hardware switches, watch your system, and roll back mistakes, all by clicking, without hand-editing configuration files.

It is written in pure Go with the [Gio](https://gioui.org) UI toolkit. No web view, no runtime other than the program itself.

> **Status:** early and actively developed. It works on the author's NixOS laptop, but expect rough edges. NASTY always test-builds your configuration before applying it, and NixOS keeps every previous generation, so mistakes are recoverable.

## What it does

| Tab | What you can do |
|---|---|
| **Packages** | Search the whole package list on the left and tick a package to queue an install. The right side lists what NASTY has installed; untick one to remove it. Nothing changes until you press **Review & Apply**. |
| **Dashboard** | Live CPU and memory graphs for the last 60 seconds. |
| **Processes** | Running programs, grouped by name. Click a group to expand it. Stop or force-kill your own processes. |
| **Hardware** | One-click switches for Bluetooth, Printing (CUPS) and Avahi network discovery. |
| **History** | Your system generations, with a rollback button. |

## How it works (the safe part)

NASTY never edits your `configuration.nix`. It writes one separate file, `/etc/nixos/nasty-config.nix`, containing the packages and switches you chose. Your own configuration imports that file.

When you press **Review & Apply**:

1. NASTY shows exactly what will be added and removed (and, if you tick the box, the exact file changes).
2. It runs a **dry build**. If that fails, your previous file is put back and nothing changes.
3. Only if the dry build passes can you press **Apply Now**, which runs `nixos-rebuild switch`.
4. You can **cancel** at any point.

You are asked for your password once per session (through `pkexec`). Only the small helper process runs as root; the window itself does not.

Settings in NASTY use `lib.mkDefault`, so anything you set by hand in `configuration.nix` always wins.

## Requirements

- NixOS with a graphical session (Wayland or X11)
- A polkit authentication agent (desktop environments such as XFCE, GNOME and KDE include one), so the password prompt can appear
- A classic, non-flake setup using `/etc/nixos/configuration.nix`. Flake-based systems are not supported yet, and NASTY warns you if it finds a `flake.nix`.

## One-time setup

Add this line to the `imports` list in `/etc/nixos/configuration.nix`:

```nix
imports = [
  ./hardware-configuration.nix
  ./nasty-config.nix   # <- add this
];
```

NASTY creates `nasty-config.nix` the first time you apply a change. NASTY warns you at startup if your configuration is missing the import line.

## Building

Get a shell with Go and the libraries Gio needs:

```
git clone https://github.com/drakkorath/NASTY.git
cd NASTY
nix-shell            # uses shell.nix
go build -o nasty
./nasty
```

(`nix develop` works too, using `flake.nix`.)

The first launch builds a local package index, which can take a minute. After that, searching is instant. Use **Update Repos** to refresh it.

## Using it

- **Install a package:** search on the left, tick it, press **Review & Apply**.
- **Remove a package:** untick it in the right-hand list ("Installed with NASTY"), then press **Review & Apply**. Ticking or unticking on the left never removes anything that is already installed.
- **Undo:** go to the **History** tab and roll back. Rollback only changes the running system, so NASTY's file still lists the packages. Apply again to bring them back, or untick them first.
- **Stop a program:** on the **Processes** tab, press **Kill** (or **Kill all** on a group), then choose **Stop** (asks the program to close) or **Force kill** (ends it immediately, and unsaved work is lost). You can only stop your own processes. System processes and NASTY itself are protected.

## Known limits

- Only packages that NASTY installed can be removed by NASTY. Packages you put in `configuration.nix` yourself are not listed.
- No flake support yet.
- The hardware tab covers three services so far.

## Project layout

| File | Purpose |
|---|---|
| `main.go` | Startup and the window loop |
| `ui.go` | Packages, Dashboard, Hardware and History screens |
| `procs.go` | The Processes tab |
| `ops.go`, `helper.go`, `client.go`, `protocol.go` | The review, apply and rollback flow and the root helper |
| `nix.go` | Writing `nasty-config.nix` and comparing configurations |
| `db.go` | Package index and settings |
| `theme.go` | Follows your system light/dark setting |

Run the tests with `go test ./...`.

## Contributing and feedback

Issues and ideas are welcome. This is a learning project, so friendly suggestions are especially appreciated.

## License

No license chosen yet. Until one is added, all rights are reserved by the author.
