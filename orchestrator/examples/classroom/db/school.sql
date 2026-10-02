-- The school database: what the portal shows and the student VM queries.
-- A replaced db VM starts again from this file: what is written at run time
-- lives in the VM's disk only.

CREATE TABLE subjects (
    id      serial PRIMARY KEY,
    name    text NOT NULL,
    teacher text NOT NULL
);

CREATE TABLE students (
    id    serial PRIMARY KEY,
    name  text NOT NULL,
    grp   text NOT NULL
);

CREATE TABLE grades (
    student_id int REFERENCES students,
    subject_id int REFERENCES subjects,
    grade      numeric(3,1) CHECK (grade BETWEEN 0 AND 10),
    PRIMARY KEY (student_id, subject_id)
);

INSERT INTO subjects (name, teacher) VALUES
    ('Networks',   'Ms. García'),
    ('Databases',  'Mr. López'),
    ('Operating Systems', 'Ms. Martín');

INSERT INTO students (name, grp) VALUES
    ('Ana',    '1A'), ('Bruno', '1A'), ('Carla', '1A'),
    ('David',  '1B'), ('Elena', '1B'), ('Fran',  '1B');

INSERT INTO grades VALUES
    (1,1,8.5),(1,2,7.0),(1,3,9.0),
    (2,1,6.0),(2,2,5.5),(2,3,7.5),
    (3,1,9.5),(3,2,8.0),(3,3,8.5),
    (4,1,4.5),(4,2,6.5),(4,3,5.0),
    (5,1,7.0),(5,2,9.0),(5,3,8.0),
    (6,1,5.5),(6,2,6.0),(6,3,6.5);

-- A view the class can start from.
CREATE VIEW report AS
    SELECT st.grp, st.name AS student, su.name AS subject, g.grade
    FROM grades g JOIN students st ON st.id = g.student_id
                  JOIN subjects su ON su.id = g.subject_id;

GRANT SELECT ON ALL TABLES IN SCHEMA public TO portal, student;
