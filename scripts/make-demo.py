#!/usr/bin/env python3
"""Generate an explicitly synthetic test fixture; never a model result or real song."""
import argparse
from pathlib import Path
from music21 import instrument, meter, note, stream, tempo


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, default=Path('data/demo'))
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=True)
    score = stream.Score()
    for name, inst, pitches in [('测试钢琴', instrument.Piano(), [60, 64, 67, 72]),
                                ('测试贝斯', instrument.ElectricBass(), [36, 36, 43, 43])]:
        part = stream.Part()
        part.partName = name
        part.insert(0, inst)
        part.insert(0, meter.TimeSignature('4/4'))
        part.insert(0, tempo.MetronomeMark(number=120))
        for pitch in pitches:
            part.append(note.Note(pitch, quarterLength=1))
        score.insert(0, part)
    score.write('musicxml', fp=str(args.output/'synthetic-fixture.musicxml'))
    score.write('midi', fp=str(args.output/'synthetic-fixture.mid'))
    print('Synthetic fixture generated (not a reference song, not a transcription result).')


if __name__ == '__main__':
    main()
