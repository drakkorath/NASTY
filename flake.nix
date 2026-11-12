{
  description = "NASTY development shell (Go + Gio system libraries)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
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
      devShells.${system}.default = pkgs.mkShell {
        nativeBuildInputs = [ pkgs.go pkgs.pkg-config ];
        buildInputs = libs;
        # Gio loads some of these at runtime.
        LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath libs;
      };
    };
}
