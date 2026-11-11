"""ies_dump - convert one .ies (client data table) to XML using the client's own
converter (GETerra...\\Converter\\ies2.py), so we read the same bytes the game does.

Usage:  python ies_dump.py <in.ies> <out.xml> [-f] [-k]
        -f  numbers are single-precision floats (default: doubles)
        -k  EUC-KR strings (default: UTF-8)
"""
import sys
from ge_paths import GAME_ROOT

sys.path.insert(0, os.path.join(GAME_ROOT, "GEAndromida", "Converter"))
import ies2  # noqa: E402


def main():
    args = [a for a in sys.argv[1:] if not a.startswith("-")]
    flags = [a for a in sys.argv[1:] if a.startswith("-")]
    src, dst = args[0], args[1]
    ies2.ies_to_xml(src, dst, None, None,
                    "EUC-KR" if "-k" in flags else "UTF-8",
                    "-f" in flags)
    print("wrote", dst)


if __name__ == "__main__":
    main()
