# Archiver: A simple archiver for backups

There are many different programs for archiving and backups. Some of
these include advanced features like deduplication/encryption etc.

This program does not do that! The more complicated the archive format
the more difficult it is to recover from corrupted backups.

Therefore this program has a different approach:

1. Use standard archiving technologies: ZIP files have been around for
   ages, are well understood and can be easily recovered in case of
   corruption.

2. If some of the ZIP file is corrupted (e.g. a bad block) the entire
   archive is not lost! It is possible to recover most of the other
   files in the archive.

This program was written to produce backups of photos, home videos
etc. The files are added to multiple ZIP files, each roughly 1Gb in
size.

In order to keep it simple, we do not break files across the ZIP files
and each ZIP file is a completely independent ZIP (we do not use
multidisk ZIPs since they are not really supported by modern
archivers).

## Reading the data

We dont necessarily want to have to unarchive all the files in order
to view a few photos. Therefore this program also adds fuse mode.

In this mode all ZIP files are combined into a single archive exported
via a fuse filesystem. Files are read on demand from the archives.
