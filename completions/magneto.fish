# Fish completion for magneto.
#
# Install by copying it into fish's completions directory:
#   cp completions/magneto.fish ~/.config/fish/completions/

complete -c magneto -f
complete -c magneto -o magnet -r -F -d 'Magnet link or batch file of links; repeat or comma-separate for multiple'
complete -c magneto -o out -x -a '(__fish_complete_directories)' -d 'Download directory'
complete -c magneto -o no-seed -x -a 'true false' -d 'Disable seeding after download'
complete -c magneto -o parallel -x -d 'Max simultaneous downloads (0 = unlimited)'
complete -c magneto -o h -d 'Show usage'
complete -c magneto -o help -d 'Show usage'
