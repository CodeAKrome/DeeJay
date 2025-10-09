#!/bin/zsh

# A script to remove duplicate music files based on the duplicates.tsv report.
# For each set of duplicates, it keeps the largest file and removes the rest.

DUPLICATES_FILE="duplicates.tsv"

if [[ ! -f "$DUPLICATES_FILE" ]]; then
    echo "Error: duplicates.tsv not found. Please run the indexer first."
    exit 1
fi

# Use awk to process the file.
# 1. Skip the header line.
# 2. Group files by their hash.
# 3. For each group, find the largest file and print the paths of all other files for deletion.
# 4. Use xargs to pass the files to 'rm' for deletion.

echo "The following files will be deleted. Review carefully."
awk -F'\t' '
NR > 1 {
    hash=$1; size=$2; path=$3;
    # Store path and size for each hash
    paths[hash] = paths[hash] path "\n";
    sizes[hash, path] = size;
    if (size > max_size[hash]) {
        max_size[hash] = size;
        largest_file[hash] = path;
    }
}
END {
    for (hash in largest_file) {
        # Split paths into an array
        split(paths[hash], path_array, "\n");
        for (i in path_array) {
            current_path = path_array[i];
            if (current_path && current_path != largest_file[hash]) {
                print current_path; # Print file to be deleted
            }
        }
    }
}' "$DUPLICATES_FILE"

echo "\nDo you want to proceed with deleting these files? (y/n)"
read -r answer

if [[ "$answer" != "y" ]]; then
    echo "Deletion cancelled."
    exit 0
fi

# Re-run awk and pipe to xargs for actual deletion
echo "Deleting files..."
awk -F'\t' '
NR > 1 {
    hash=$1; size=$2; path=$3;
    paths[hash] = paths[hash] path "\n";
    sizes[hash, path] = size;
    if (size > max_size[hash]) {
        max_size[hash] = size;
        largest_file[hash] = path;
    }
}
END {
    for (hash in largest_file) {
        split(paths[hash], path_array, "\n");
        for (i in path_array) {
            current_path = path_array[i];
            if (current_path && current_path != largest_file[hash]) {
                # Use print0 for safe handling of filenames with spaces
                printf "%s\0", current_path;
            }
        }
    }
}' "$DUPLICATES_FILE" | xargs -0 rm

echo "Duplicate files removed."
