from django.db import models

# Create your models here.

def dynamic_save_path(instance,filename):
    return f"uploads/{instance.title}/{filename}" 

class FileModel(models.Model):
    title = models.CharField(max_length=50)
    file = models.FileField(upload_to=dynamic_save_path)
    created_on = models.DateTimeField(auto_now=True)
    
    def __str__(self):
        return self.title
