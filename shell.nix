{ pkgs ? import <nixpkgs> {} }:
pkgs.mkShell {
  nativeBuildInputs = [ pkgs.go pkgs.pkg-config ];
  buildInputs = with pkgs; [
    wayland libxkbcommon libGL vulkan-headers vulkan-loader
    libx11 libxcursor libxfixes libxcb
  ];
  LD_LIBRARY_PATH = pkgs.lib.makeLibraryPath (with pkgs; [
    wayland libxkbcommon libGL vulkan-loader
    libx11 libxcursor libxfixes libxcb
  ]);
}
