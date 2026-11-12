{
  description = "NASTY - NixOS Visual Control Center";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };

      # Libraries Gio needs to draw a window.
      libs = with pkgs; [
        wayland
        libxkbcommon
        libGL
        vulkan-headers
        vulkan-loader
        libx11
        libxcursor
        libxfixes
        libxcb
      ];
    in {
      # nix build          -> ./result/bin/nasty
      # nix run github:drakkorath/NASTY
      packages.${system}.default = pkgs.buildGoModule {
        pname = "nasty";
        version = "0.1.0";
        src = ./.;

        # Checksum of the Go dependencies. If go.mod/go.sum change, set this to
        # pkgs.lib.fakeHash, run `nix build`, and paste the "got:" value here.
        vendorHash = "sha256-jcESGnc+I7uhk6mrC4YnQpA1UHM0Bov40cbh6sYIDVQ=";

        nativeBuildInputs = [ pkgs.pkg-config pkgs.makeWrapper pkgs.copyDesktopItems ];

        # Adds NASTY to app menus and launchers (rofi drun, Quickshell, XFCE...).
        # It is only a file in share/applications; launchers choose what to show.
        desktopItems = [
          (pkgs.makeDesktopItem {
            name = "nasty";
            desktopName = "NASTY";
            genericName = "NixOS Control Center";
            comment = "Install packages and manage your NixOS system with clicks";
            exec = "nasty";
            icon = "preferences-system";
            terminal = false;
            categories = [ "System" "Settings" ];
            keywords = [ "nixos" "packages" "settings" "system" ];
          })
        ];
        buildInputs = libs;

        # Gio loads some graphics libraries while running, so tell the
        # program where they are.
        postFixup = ''
          wrapProgram $out/bin/nasty \
            --prefix LD_LIBRARY_PATH : ${pkgs.lib.makeLibraryPath libs}
        '';

        meta = {
          description = "Click-to-configure control center for NixOS";
          homepage = "https://github.com/drakkorath/NASTY";
          license = pkgs.lib.licenses.mit;
          mainProgram = "nasty";
          platforms = [ "x86_64-linux" ];
        };
      };

      apps.${system}.default = {
        type = "app";
        program = "${self.packages.${system}.default}/bin/nasty";
      };

      # nix develop -> a shell for working on NASTY
      devShells.${system}.default = pkgs.mkShell {
        nativeBuildInputs = [ pkgs.go pkgs.pkg-config ];
        buildInputs = libs;
        LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath libs;
      };
    };
}
