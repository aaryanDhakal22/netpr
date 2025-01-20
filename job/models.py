from django.db import models

# Create your models here.
class FileModel(models.Model):
    title = models.CharField(max_length=50)
    file = models.FileField()
    ext = models.CharField(max_length=10)
    created_on = models.DateTimeField(auto_now=True)
    save_path = "../print_files/"+str(title)+str(ext)
    
