# Generated by Django 5.1.5 on 2025-01-20 23:09

from django.db import migrations, models


class Migration(migrations.Migration):

    dependencies = [
        ('job', '0001_initial'),
    ]

    operations = [
        migrations.AlterField(
            model_name='filemodel',
            name='file',
            field=models.FileField(upload_to='../print_files/<django.db.models.fields.CharField>'),
        ),
    ]
